package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	mcpserver "deeptalk/common/mcp/server"
)

func main() {
	httpAddr := flag.String("http-addr", ":8081", "HTTP服务器地址")
	stdio := flag.Bool("stdio", false, "以 stdio 方式运行（供 MCP 客户端作为子进程启动）")
	flag.Parse()

	if *stdio {
		if err := mcpserver.StartStdioServer(context.Background()); err != nil {
			log.Fatalf("stdio 服务器错误: %v", err)
		}
		return
	}

	fmt.Printf("启动MCP天气服务器，监听 %s/mcp\n", *httpAddr)
	if err := mcpserver.StartServer(*httpAddr); err != nil {
		log.Fatalf("服务器错误: %v", err)
	}
}
