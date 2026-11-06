// 发布工具：给新版本签名并上传，已安装的程序之后会自动收到更新。
//
//	go run ./tools/release keygen                    生成签名密钥（只做一次）
//	go run ./tools/release publish -notes "更新内容"  发布 dist/BatchGen.exe
//
// 通常直接运行 release.sh，它会先编译打包再调用这里。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultKey = `F:\Keys\batchgen-update.key`
	repo       = "Roins-hub/batchgen" // 公开仓库，要和 updater.go 的 updateURL 一致
)

func main() {
	if len(os.Args) < 2 {
		die(errors.New("用法：release keygen | release publish -notes 更新内容"))
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "publish":
		err = publish(os.Args[2:])
	default:
		err = fmt.Errorf("未知命令 %s", os.Args[1])
	}
	if err != nil {
		die(err)
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "失败：", err)
	os.Exit(1)
}

func keygen() error {
	path := firstEnv("BATCHGEN_RELEASE_KEY", defaultKey)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在。换密钥后旧版本将无法自动更新，确实要换请先手动移走它", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println("私钥已保存：", path)
	fmt.Println("公钥（写进 updater.go 的 updatePublicKey）：", base64.StdEncoding.EncodeToString(pub))
	return nil
}

func firstEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func loadPrivateKey() (ed25519.PrivateKey, error) {
	path := firstEnv("BATCHGEN_RELEASE_KEY", defaultKey)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取签名私钥失败：%w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s 不是有效的签名私钥", path)
	}
	return ed25519.PrivateKey(key), nil
}

func productVersion() (string, error) {
	raw, err := os.ReadFile("winres/winres.json")
	if err != nil {
		return "", err
	}
	var v struct {
		Version struct {
			One struct {
				Lang struct {
					Info struct {
						CN struct {
							ProductVersion string
						} `json:"0804"`
					} `json:"info"`
				} `json:"0000"`
			} `json:"#1"`
		} `json:"RT_VERSION"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v.Version.One.Lang.Info.CN.ProductVersion, nil
}

func newerThan(a, b string) bool {
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(x), len(y)) {
		var m, n int
		if i < len(x) {
			m, _ = strconv.Atoi(x[i])
		}
		if i < len(y) {
			n, _ = strconv.Atoi(y[i])
		}
		if m != n {
			return m > n
		}
	}
	return false
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	notes := fs.String("notes", "", "这个版本的更新内容，用户会看到")
	fs.Parse(args)
	if strings.TrimSpace(*notes) == "" {
		return errors.New("请用 -notes 写上更新内容")
	}
	version, err := productVersion()
	if err != nil || version == "" {
		return fmt.Errorf("读取版本号失败：%v", err)
	}
	priv, err := loadPrivateKey()
	if err != nil {
		return err
	}
	if err := checkNotPublished(version); err != nil {
		return err
	}
	exe, err := os.ReadFile("dist/BatchGen.exe")
	if err != nil {
		return err
	}
	zip, err := os.ReadFile(filepath.Join("release", fmt.Sprintf("BatchGen-%s-win-x64.zip", version)))
	if err != nil {
		return err
	}
	setup, err := os.ReadFile(filepath.Join("release", fmt.Sprintf("BatchGen-%s-Setup.exe", version)))
	if err != nil {
		return err
	}

	tag := "v" + version
	sum := sha256.Sum256(exe)
	manifest, _ := json.Marshal(map[string]any{
		"version": version, "url": fmt.Sprintf("https://github.com/%s/releases/download/%s/BatchGen.exe", repo, tag),
		"sha256": hex.EncodeToString(sum[:]), "size": len(exe), "notes": strings.TrimSpace(*notes),
	})
	signed, _ := json.MarshalIndent(map[string]string{
		"manifest":  string(manifest),
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)),
	}, "", "  ")

	// 附件名要固定：程序和新用户都通过 releases/latest/download/文件名 访问。
	dir, err := os.MkdirTemp("", "batchgen-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	assets := map[string][]byte{"BatchGen.exe": exe, "BatchGen-win-x64.zip": zip, "BatchGen-Setup.exe": setup, "latest.json": signed}
	var files []string
	for name, data := range assets {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
		files = append(files, p)
	}
	cmdArgs := []string{"release", "create", tag, "--repo", repo, "--title", "BatchGen " + version, "--notes", strings.TrimSpace(*notes), "--latest"}
	cmd := exec.Command("gh", append(cmdArgs, files...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("发布到 GitHub 失败：%w", err)
	}
	fmt.Printf("已发布 %s。\n给新用户的下载地址：https://github.com/%s/releases/latest/download/BatchGen-Setup.exe\n", version, repo)
	return nil
}

// checkNotPublished 防止忘记改版本号：线上已有相同或更高的版本时拒绝。
func checkNotPublished(version string) error {
	out, err := exec.Command("gh", "api", "repos/"+repo+"/releases/latest", "--jq", ".tag_name").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "404") {
			return nil // 第一次发布
		}
		return fmt.Errorf("读取线上版本失败：%w", err)
	}
	cur := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	if !newerThan(version, cur) {
		return fmt.Errorf("线上已经是 %s，要发布的是 %s。请先改 winres/winres.json 里的版本号", cur, version)
	}
	return nil
}
