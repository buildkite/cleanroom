package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("cleanroom-vmnet-echo", flag.ContinueOnError)
	flags.SetOutput(stderr)

	listenAddr := flags.String("listen", ":18080", "tcp listen address")
	response := flags.String("response", "ok\n", "response written to the first client")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "listen %q: %v\n", *listenAddr, err)
		return 1
	}
	if err := serve(listener, *response); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func serve(listener net.Listener, response string) error {
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept %q: %w", listener.Addr().String(), err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, response); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}
