package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
)

func main() {
	listenAddr := flag.String("listen", ":18080", "tcp listen address")
	response := flag.String("response", "ok\n", "response written to the first client")
	flag.Parse()

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %q: %v\n", *listenAddr, err)
		os.Exit(1)
	}
	defer listener.Close()

	conn, err := listener.Accept()
	if err != nil {
		fmt.Fprintf(os.Stderr, "accept %q: %v\n", *listenAddr, err)
		os.Exit(1)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, *response); err != nil {
		fmt.Fprintf(os.Stderr, "write response: %v\n", err)
		os.Exit(1)
	}
}
