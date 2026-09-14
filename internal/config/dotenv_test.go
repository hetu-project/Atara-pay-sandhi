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
