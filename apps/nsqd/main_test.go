package main

import (
	"crypto/tls"
	"os"
	"testing"

	"nsq/internal/test"
	"nsq/nsqd"

	"github.com/BurntSushi/toml"
	"github.com/mreiferson/go-options"
)

func TestConfigFlagParsing(t *testing.T) {
	opts := nsqd.NewOptions()
	opts.Logger = test.NewTestLogger(t)

	flagSet := nsqdFlagSet(opts)
	flagSet.Parse([]string{}) //nolint

	var cfg config
	f, err := os.Open("../../contrib/nsqd.cfg.example")
	if err != nil {
		t.Fatalf("%s", err)
	}
	toml.DecodeReader(f, &cfg) //nolint
	cfg.Validate()

	options.Resolve(opts, flagSet, cfg)
	nsqd.New(opts) //nolint

	if opts.TLSMinVersion != tls.VersionTLS10 {
		t.Errorf("min %#v not expected %#v", opts.TLSMinVersion, tls.VersionTLS10)
	}
}
