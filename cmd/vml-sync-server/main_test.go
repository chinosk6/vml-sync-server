package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

// 启动参数：默认值、环境变量、命令行参数优先于环境变量。
func TestParseFlags(t *testing.T) {
	for _, name := range []string{"VML_LISTEN", "VML_DATA_DIR", "VML_TLS_CERT", "VML_TLS_KEY"} {
		t.Setenv(name, "")
	}

	o, err := parseFlags(nil)
	if err != nil || o.listen != ":8080" || o.dataDir != "./data" || o.tlsCert != "" || o.tlsKey != "" || o.showVersion || o.resetAdmin {
		t.Fatalf("defaults = %+v, %v", o, err)
	}

	t.Setenv("VML_LISTEN", "127.0.0.1:9000")
	t.Setenv("VML_DATA_DIR", "/srv/vml")
	t.Setenv("VML_TLS_CERT", "/etc/cert.pem")
	t.Setenv("VML_TLS_KEY", "/etc/key.pem")
	o, err = parseFlags(nil)
	if err != nil || o.listen != "127.0.0.1:9000" || o.dataDir != "/srv/vml" || o.tlsCert != "/etc/cert.pem" || o.tlsKey != "/etc/key.pem" {
		t.Fatalf("from the environment = %+v, %v", o, err)
	}

	// 命令行参数优先。
	o, err = parseFlags([]string{"-listen", ":1234", "-data", "elsewhere"})
	if err != nil || o.listen != ":1234" || o.dataDir != "elsewhere" || o.tlsCert != "/etc/cert.pem" {
		t.Fatalf("flags over the environment = %+v, %v", o, err)
	}

	o, err = parseFlags([]string{"-version"})
	if err != nil || !o.showVersion {
		t.Fatalf("-version = %+v, %v", o, err)
	}
	o, err = parseFlags([]string{"-reset-admin-password", "-data", "d"})
	if err != nil || !o.resetAdmin || o.dataDir != "d" {
		t.Fatalf("-reset-admin-password = %+v, %v", o, err)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	for _, name := range []string{"VML_LISTEN", "VML_DATA_DIR", "VML_TLS_CERT", "VML_TLS_KEY"} {
		t.Setenv(name, "")
	}

	// 还带着 -config 启动：一句看得懂的话，说明配置已经搬到管理页里了。
	for _, args := range [][]string{{"-config", "config.yaml"}, {"-config=config.yaml"}, {"-config", ""}} {
		_, err := parseFlags(args)
		if err == nil || !strings.Contains(err.Error(), "no config file any more") ||
			!strings.Contains(err.Error(), "admin page") || !strings.Contains(err.Error(), "-h") {
			t.Fatalf("%v: %v", args, err)
		}
	}

	for name, args := range map[string][]string{
		"only the certificate": {"-tls-cert", "cert.pem"},
		"only the key":         {"-tls-key", "key.pem"},
		"stray argument":       {"config.yaml"},
		"empty listen":         {"-listen", ""},
		"empty data":           {"-data", ""},
		"unknown flag":         {"-no-such-flag"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseFlags([]string{"-tls-cert", "c", "-tls-key", "k"}); err != nil {
		t.Fatalf("certificate and key together: %v", err)
	}

	if _, err := parseFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v", err)
	}
}
