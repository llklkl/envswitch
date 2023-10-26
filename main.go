package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
)

func handle(conn net.Conn) {
	defer func() {
		conn.Close()
	}()
	reader := bufio.NewReader(conn)
	var dstAddr string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		fmt.Println(line)
		if dstAddr == "" {
			if len(line) > 7 && strings.EqualFold(line[:7], "connect") {
				sps := strings.SplitN(line, " ", 3)
				if len(sps) == 3 {
					dstAddr = sps[1]
				}
			}
		}
		if len(line) == 0 {
			break
		}
	}

	remoteConn, err := net.Dial("tcp", dstAddr)
	if err != nil {
		return
	}
	_, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if err != nil {
		return
	}
	defer func() {
		remoteConn.Close()
	}()

	errChan := make(chan error, 1)
	go func() {
		for {
			_, err := io.Copy(conn, remoteConn)
			if err != nil {
				errChan <- err
				return
			}
		}
	}()
	go func() {
		for {
			_, err := io.Copy(remoteConn, reader)
			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	err = <-errChan
	return
}

func main() {
	lis, err := net.Listen("tcp", ":8088")
	if err != nil {
		fmt.Println(err)
		return
	}

	for {
		conn, err := lis.Accept()
		if err != nil {
			fmt.Println(err)
			break
		}

		go handle(conn)
	}
}
