package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct{ in, k, v string }{
		{`A=1`, "A", "1"},
		{`  A = 1 `, "A", "1"},
		{`export A=1`, "A", "1"},
		{`A="a b"`, "A", "a b"},
		{`A='a b'`, "A", "a b"},
		{`A=`, "A", ""},
		{`A=x=y`, "A", "x=y"}, // 值里的等号不再拆
		{`A="unbalanced`, "A", `"unbalanced`},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if !ok || k != c.k || v != c.v {
			t.Errorf("parseLine(%q) = (%q, %q, %v), 想要 (%q, %q, true)", c.in, k, v, ok, c.k, c.v)
		}
	}
	for _, skip := range []string{``, `   `, `# comment`, `  # comment`, `=novalue`, `nokey`} {
		if _, _, ok := parseLine(skip); ok {
			t.Errorf("parseLine(%q) 不该被当成一条配置", skip)
		}
	}
}

// 命令行显式给的值必须赢过文件。反过来的话「我明明改了端口怎么没生效」
// 会查很久——这条是这个加载器唯一容易搞反的地方。
func TestDotenvDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FROM_FILE=file\nALREADY_SET=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATARA_ENV_FILE", path)
	t.Setenv("ALREADY_SET", "env")

	loadDotenv()

	if got := os.Getenv("FROM_FILE"); got != "file" {
		t.Errorf("文件里的新键没读进来：%q", got)
	}
	if got := os.Getenv("ALREADY_SET"); got != "env" {
		t.Errorf("文件盖掉了环境里已有的值：%q，想要 %q", got, "env")
	}
}

// 没有 .env 是正常状态，不是错误。
func TestDotenvMissingFileIsFine(t *testing.T) {
	t.Setenv("ATARA_ENV_FILE", filepath.Join(t.TempDir(), "nope"))
	loadDotenv()
}

// envInt 与 envPosInt 的规则是相反的，必须各自成立。
//
// 这两条曾经被合成过一个函数，结果是 DEEPSEEK_MAX_TOKENS=0 会被原样发给上游，
// 每次回答截成空；把守卫加回去又会让 IDANALYZER_MODE=0 退回默认——而 0 是它
// 的合法取值。合成一个就必然有一边是错的，这个测试钉住这件事。
func TestEnvIntKeepsZero(t *testing.T) {
	t.Setenv("ATARA_TEST_INT", "0")
	if got := envInt("ATARA_TEST_INT", 7); got != 0 {
		t.Fatalf("0 是合法取值，不该退回默认: %d", got)
	}
	t.Setenv("ATARA_TEST_INT", "-3")
	if got := envInt("ATARA_TEST_INT", 7); got != -3 {
		t.Fatalf("负数也该原样收下: %d", got)
	}
}

func TestEnvPosIntRejectsNonPositive(t *testing.T) {
	for _, v := range []string{"0", "-1", "abc"} {
		t.Setenv("ATARA_TEST_POS", v)
		if got := envPosInt("ATARA_TEST_POS", 800); got != 800 {
			t.Fatalf("%q 应当当成没配退回 800，得到 %d", v, got)
		}
	}
	t.Setenv("ATARA_TEST_POS", "1200")
	if got := envPosInt("ATARA_TEST_POS", 800); got != 1200 {
		t.Fatalf("正数该收下: %d", got)
	}
}

// 没设就是没设，两个都该给默认值。
func TestEnvIntUnset(t *testing.T) {
	if envInt("ATARA_TEST_UNSET_X", 5) != 5 || envPosInt("ATARA_TEST_UNSET_X", 5) != 5 {
		t.Fatal("没配时没给默认值")
	}
}
