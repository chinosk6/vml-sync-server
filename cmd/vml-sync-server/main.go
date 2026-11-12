// vml-sync-server 是模型同步服务的入口。
//
// 没有配置文件：启动时只有监听地址、数据目录（和可选的 HTTPS 证书）这几个参数，
// 其余所有设置都在管理页里改，存在数据目录里（docs/API.md 第 6 节）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/httpapi"
	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/version"
	"vml-sync-server/web"
)

// options 是全部启动参数。命令行参数优先于环境变量。
type options struct {
	listen  string
	dataDir string
	tlsCert string
	tlsKey  string

	showVersion bool
	resetAdmin  bool
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// parseFlags 解析命令行。出错时返回的错误已经是给人看的说明。
func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("vml-sync-server", flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", envOr("VML_LISTEN", ":8080"), "address to listen on, e.g. 127.0.0.1:8080 (env VML_LISTEN)")
	fs.StringVar(&o.dataDir, "data", envOr("VML_DATA_DIR", "./data"), "data directory: settings, metadata, temporary uploads and locally stored models (env VML_DATA_DIR)")
	fs.StringVar(&o.tlsCert, "tls-cert", os.Getenv("VML_TLS_CERT"), "optional: certificate file, to serve HTTPS directly (env VML_TLS_CERT)")
	fs.StringVar(&o.tlsKey, "tls-key", os.Getenv("VML_TLS_KEY"), "optional: private key file for -tls-cert (env VML_TLS_KEY)")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&o.resetAdmin, "reset-admin-password", false, "generate a new admin password, print it and exit (stop the server first)")
	// 0.1.0 用配置文件启动；现在已经没有了，留着这个参数只为了给一句看得懂的话。
	oldConfig := fs.String("config", "", "removed: there is no config file any more")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vml-sync-server [flags]")
		fmt.Fprintln(fs.Output(), "\nThere is no config file. Everything except the flags below is changed in the admin page")
		fmt.Fprintln(fs.Output(), "({listen address}{base_path}/admin/) and stored in the data directory.")
		fmt.Fprintln(fs.Output(), "\nFlags (each can also be given through the environment variable shown):")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}

	configGiven := *oldConfig != ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configGiven = true
		}
	})
	if configGiven {
		return o, errors.New("-config is gone: there is no config file any more. All settings are now changed in the admin page " +
			"and stored in the data directory. Start the program without -config (run it with -h to see the startup flags); " +
			"the admin password for the first login is printed in the log and saved in the data directory")
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q (run with -h to see the startup flags)", fs.Arg(0))
	}
	if o.listen == "" {
		return o, errors.New("-listen must not be empty")
	}
	if o.dataDir == "" {
		return o, errors.New("-data must not be empty")
	}
	if (o.tlsCert == "") != (o.tlsKey == "") {
		return o, errors.New("-tls-cert and -tls-key must be given together (or not at all)")
	}
	return o, nil
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "vml-sync-server:", err)
		os.Exit(2)
	}
	if opts.showVersion {
		fmt.Println(version.Name, version.Version)
		return
	}

	// 日志带时间，打到标准输出。
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	// 数据目录的相对路径相对于启动时的当前目录。
	if abs, err := filepath.Abs(opts.dataDir); err == nil {
		opts.dataDir = abs
	}

	run := serve
	if opts.resetAdmin {
		run = resetAdminPassword
	}
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "vml-sync-server:", err)
		os.Exit(1)
	}
}

// openDB 建好数据目录并打开里面的元数据库。
func openDB(dataDir string) (*meta.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the data directory: %w", err)
	}
	return meta.Open(filepath.Join(dataDir, "meta.db"))
}

// resetAdminPassword 实现 -reset-admin-password。
func resetAdminPassword(opts options) error {
	db, err := openDB(opts.dataDir)
	if err != nil {
		return fmt.Errorf("%w\n(the server has to be stopped before resetting the admin password)", err)
	}
	defer db.Close()
	password, err := app.ResetAdminPassword(db, opts.dataDir)
	if err != nil {
		return err
	}
	fmt.Println("New admin password:", password)
	fmt.Println("It has also been written to", app.InitialPasswordPath(opts.dataDir))
	fmt.Println("Start the server, log in with it and change it in the admin page.")
	return nil
}

// serve 是正常的启动流程。
func serve(opts options) error {
	db, err := openDB(opts.dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	core, err := app.New(db, app.Options{DataDir: opts.dataDir})
	if err != nil {
		return err
	}

	passwordFile := app.InitialPasswordPath(opts.dataDir)
	if password := core.FirstRunPassword(); password != "" {
		// 初始密码只在生成它的这一次启动里打印。
		log.Printf("first start: generated a client key and an initial admin password")
		log.Printf("initial admin password: %s", password)
		log.Printf("it is also saved in %s; log in to the admin page with it and change it there", passwordFile)
	} else if core.InitialPassword() {
		log.Printf("reminder: the admin password is still the generated one (see %s); change it in the admin page", passwordFile)
	}

	// 收到中断 / 终止信号后开始优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 开始接受请求之前先让记录和存储一致。存储用不了也照常启动：管理页得能进，才能把设置改对。
	activateCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	err = core.Activate(activateCtx)
	cancel()
	if err != nil {
		log.Printf("warning: the configured storage is not usable: %v", err)
		log.Printf("warning: starting anyway so the storage settings can be fixed in the admin page; " +
			"client uploads and downloads answer 503 until the storage works (retried in the background)")
	}

	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", opts.listen, err)
	}
	server := &http.Server{
		Handler: httpapi.New(core, web.Admin(), httpapi.Options{}),
		// 上传和下载可能持续很久，所以不设整体的读写超时；
		// 各个处理函数自己按“有没有数据在流动”来掐断卡住的连接。
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		core.Run(ctx)
	}()

	scheme := "http"
	if opts.tlsCert != "" {
		scheme = "https"
	}
	set := core.Settings()
	// 不打印任何密钥和密码。
	log.Printf("%s %s listening on %s (%s), data directory %s, base_path=%q, storage=%s, transfer=%s",
		version.Name, version.Version, listener.Addr(), scheme, opts.dataDir, set.BasePath, set.Storage.Type, set.Transfer)

	serveErr := make(chan error, 1)
	go func() {
		if opts.tlsCert != "" {
			serveErr <- server.ServeTLS(listener, opts.tlsCert, opts.tlsKey)
		} else {
			serveErr <- server.Serve(listener)
		}
	}()

	select {
	case err := <-serveErr:
		stop()
		background.Wait()
		core.Close()
		return err
	case <-ctx.Done():
	}

	log.Printf("shutting down")
	// 给进行中的请求一点时间收尾，到点还没完的直接断开。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		server.Close()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("error: %v", err)
	}
	background.Wait()
	// 没传完的上传会话到此作废：清掉临时文件，中止对象存储上没完成的分片上传。
	core.Close()
	log.Printf("stopped")
	return nil
}
