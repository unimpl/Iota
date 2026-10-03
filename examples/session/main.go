package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// main 展示会话保存、关闭、恢复，以及使用新 Agent 继续同一段对话。
// 需要设置 IOTA_MODEL；认证及兼容接口地址与 basic 示例相同。
func main() {
	dir := flag.String("dir", "./sessions", "directory for example session files")
	flag.Parse()
	if err := run(*dir); err != nil {
		log.Fatal(err)
	}
}

// run 返回错误后 main 才退出，确保失败路径也会关闭会话文件。
func run(dir string) (runErr error) {
	provider, err := openaicompat.New(openaicompat.Config{
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
	})
	if err != nil {
		return err
	}
	config := iota.Config{Provider: provider, Model: os.Getenv("IOTA_MODEL")}
	agent, err := iota.New(config)
	if err != nil {
		return err
	}

	// SDK 的保存目录由调用方选择，直接使用 Agent.Run 不会保存。
	session, err := iota.NewSession(dir, iota.SessionInfo{Model: config.Model})
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, session.Close()) }()
	fmt.Fprintln(os.Stderr, "session:", session.Path())
	result, err := session.Run(context.Background(), agent, "Remember this number: 42. Reply briefly.", nil)
	if err != nil {
		return err
	}
	fmt.Println(result.Text)
	if err := session.Close(); err != nil {
		return err
	}

	// 新 Agent 没有旧历史，模拟程序重启后重新配置模型服务。
	resumedAgent, err := iota.New(config)
	if err != nil {
		return err
	}
	resumedSession, err := iota.OpenSession(session.Path())
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, resumedSession.Close()) }()
	// 打开文件后先恢复历史，再继续运行；结果追加到原来的文件。
	if err := resumedSession.Restore(resumedAgent); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "restored %d messages\n", len(resumedAgent.Messages()))
	result, err = resumedSession.Run(context.Background(), resumedAgent, "What number did I ask you to remember?", nil)
	if err != nil {
		return err
	}
	fmt.Println(result.Text)
	return nil
}
