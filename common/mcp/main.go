package main

import (
	"flag"
	"fmt"
	"log"

	mcpserver "deeptalk/common/mcp/server"
)

func main() {
	httpAddr := flag.String("http-addr", ":8081", "HTTP服务器地址")
	flag.Parse()

	fmt.Printf("启动MCP天气服务器，监听 %s/mcp\n", *httpAddr)
	if err := mcpserver.StartServer(*httpAddr); err != nil {
		log.Fatalf("服务器错误: %v", err)
	}
}
