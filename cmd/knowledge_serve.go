package cmd

import (
	"fmt"
	"github.com/tszaks/pallium/internal/gitlog"
	"github.com/tszaks/pallium/internal/knowledge"
	"io"
	"strconv"
)

func runKnowledgeServe(out io.Writer, args []string) error {
	port := 8766
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" {
			if i+1 >= len(args) {
				return fmt.Errorf("--port needs a number")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 || n > 65535 {
				return fmt.Errorf("invalid port")
			}
			port = n
		} else {
			positional = append(positional, args[i])
		}
	}
	root, err := gitlog.RepoRoot(optionalRepoArg(positional, 0))
	if err != nil {
		return err
	}
	listener, server, err := knowledge.ServeBrowser(root, port)
	if err != nil {
		return err
	}
	defer listener.Close()
	fmt.Fprintf(out, "Pallium knowledge: http://%s\n", listener.Addr())
	return server.Serve(listener)
}
