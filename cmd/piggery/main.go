// Command piggery: `piggery serve` (daemon), otherwise the CLI (`piggery mcp` included).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sting8k/piggery/internal/cli"
	"github.com/sting8k/piggery/internal/server"
)

func main() {
	dir, err := server.DefaultDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "piggery:", err)
		os.Exit(1)
	}
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := server.Run(ctx, server.Config{Dir: dir, Version: cli.Version}); err != nil {
			fmt.Fprintln(os.Stderr, "piggery serve:", err)
			os.Exit(1)
		}
	default:
		os.Exit(cli.Main(dir, args, os.Stdout, os.Stderr))
	}
}
