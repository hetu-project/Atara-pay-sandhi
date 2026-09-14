package config

import (
	"bufio"
	"os"
	"strings"
)

// EnvFile 是自动加载的那份本地配置。改用别处就设 ATARA_ENV_FILE。
const EnvFile = ".env"

// loadDotenv 把 .env 里的键值读进进程环境。
//
// **已经设了的不覆盖**：命令行上显式给的值永远赢过文件。这条是故意的，
// 否则「我明明在命令行改了端口，怎么还是老的」会查很久。
//
// 文件不存在就什么都不做——没有 .env 是正常状态，不是错误。
//
// 注意这里读的和 Makefile 里 run-chain 的 `set -a && . ./.env` 是同一份文件。
// 两边都读不冲突：那边先进环境，这边看到已设就跳过。
func loadDotenv() {
	path := os.Getenv("ATARA_ENV_FILE")
	if path == "" {
		path = EnvFile
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := parseLine(s.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(k); set {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

// parseLine 拆一行 KEY=VALUE。容得下 `export K=v`、行首缩进、两侧空白，
// 以及把值整个引起来的写法——私钥和 URL 里常有让 shell 不痛快的字符，
// 人会习惯性加引号。
func parseLine(line string) (key, val string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "export ")

	i := strings.IndexByte(t, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:i])
	val = strings.TrimSpace(t[i+1:])

	// 引号只脱最外面一层，成对才脱。值里本来就带引号的不动它。
	if len(val) >= 2 {
		if q := val[0]; (q == '"' || q == '\'') && val[len(val)-1] == q {
			val = val[1 : len(val)-1]
		}
	}
	if key == "" {
		return "", "", false
	}
	return key, val, true
}
