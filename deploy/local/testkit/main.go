// Command testkit holds the small test servers of the local stack
// (docs/self-hosting/local-testing.md). It is one binary with subcommands:
//
//	testkit pod-identity-agent -listen 127.0.0.1:2703 -upstream URL -ca FILE
//	testkit claude -p --output-format stream-json ...
//
// The subcommand claude is a stand-in for the claude CLI (see claude.go).
// When the binary has the name claude, it is the stand-in. The tool image of
// the stand-in installs it as /opt/openfactory/tool/bin/claude.
//
// The image of the binary also holds git, so that `git daemon` can serve the
// test repositories. The binary is for tests only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if filepath.Base(os.Args[0]) == "claude" {
		passThrough(os.Args)
		os.Exit(runClaude(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: testkit <command> [flags]")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  pod-identity-agent  forward EKS Pod Identity credential requests to a broker")
	fmt.Fprintln(w, "  claude              a stand-in for the claude CLI with fixed answers")
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "pod-identity-agent":
		return runAgent(args[1:], stderr)
	case "claude":
		return runClaude(args[1:], os.Stdin, os.Stdout, stderr)
	case "-h", "-help", "--help", "help":
		usage(stderr)
		return 0
	}
	fmt.Fprintf(stderr, "testkit: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func runAgent(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("pod-identity-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:2703", "address to listen on")
	upstream := fs.String("upstream", "", "base URL of the credential broker (HTTPS), without /v1/credentials")
	ca := fs.String("ca", "", "PEM file with the CA that signed the broker certificate (system roots stay trusted)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	logger := log.New(stderr, "pod-identity-agent: ", log.LstdFlags)
	h, err := newAgentHandler(*upstream, *ca, logger)
	if err != nil {
		logger.Print(err)
		return 2
	}
	return serve(*listen, h, logger)
}

// serve runs h until SIGINT or SIGTERM.
func serve(addr string, h http.Handler, logger *log.Logger) int {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Printf("listening on %s", addr)
	select {
	case err := <-errc:
		logger.Print(err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return 0
}
