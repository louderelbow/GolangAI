// Package rag 提供文档切片、索引与混合检索能力。
package rag

import (
	"context"
	"deeptalk/internal/infra/config"
	redisPkg "deeptalk/internal/infra/redis"
	"fmt"
	"os"

	redisIndexer "github.com/cloudwego/eino-ext/components/indexer/redis"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/schema"
)

type RAGIndexer struct {
	embedding embedding.Embedder
	indexer   *redisIndexer.Indexer
}

// NewRAGIndexer 构建知识库索引。
//
// 注意：向量模型固定取 ragModelConfig.embeddingModel（走进程内共享的 embedding 客户端），
// 不再由调用方传入——传进来的模型名如果和配置不一致，会导致"建索引用 A 模型、
// 查询用 B 模型"，检索结果毫无意义，索性去掉这个参数避免误用。
func NewRAGIndexer(filename string) (*RAGIndexer, error) {

	// 用于控制整个初始化流程（超时 / 取消等），这里先用默认背景即可
	ctx := context.Background()

	// 从配置读取 Embedding API Key
	conf := config.GetConfig()

	// 向量的维度大小（等于向量模型输出的数字个数）
	dimension := conf.RagModelConfig.RagDimension

	// 1. 复用进程内共享的"向量生成器"（Embedding）
	// 每次新建都很贵：内部会读 volcengine 的 ini 配置文件，而且新 client 意味着
	// 新的连接池、HTTP keep-alive 失效。详见 client.go 的说明。
	embedder, err := SharedEmbedder()
	if err != nil {
		return nil, err
	}

	// 2. 初始化 Redis 中的向量索引结构

	if err := redisPkg.InitRedisIndex(ctx, filename, dimension); err != nil {
		return nil, fmt.Errorf("failed to init redis index: %w", err)
	}

	// 获取 Redis 客户端，用于后续数据写入
	rdb := redisPkg.Rdb

	// 3. 配置索引器（定义：文档如何被存进 Redis）
	indexerConfig := &redisIndexer.IndexerConfig{
		Client:    rdb,                                        // Redis 客户端
		KeyPrefix: redisPkg.GenerateIndexNamePrefix(filename), // 不同知识库使用不同前缀，避免冲突
		BatchSize: 10,                                         // 批量处理文档，提高写入效率

		// 定义：一段文档（Document）在 Redis 中该如何存储
		DocumentToHashes: func(ctx context.Context, doc *schema.Document) (*redisIndexer.Hashes, error) {

			// 从文档的元数据中取出来源信息（例如文件名、URL）
			source := ""
			if s, ok := doc.MetaData["source"].(string); ok {
				source = s
			}

			// 构造 Redis 中实际存储的数据结构（Hash）
			return &redisIndexer.Hashes{
				// Redis Key，一般由“知识库名 + 文档块 ID”组成
				Key: fmt.Sprintf("%s:%s", filename, doc.ID),

				// Redis Hash 中的字段
				Field2Value: map[string]redisIndexer.FieldValue{
					// content：原始文本内容
					// EmbedKey 表示：该字段需要先做向量化，
					// 生成的向量会存入名为 "vector" 的字段中
					"content": {Value: doc.Content, EmbedKey: "vector"},

					// metadata：一些辅助信息，不参与向量计算
					"metadata": {Value: source},
				},
			}, nil
		},
	}

	// 将“向量生成器”交给索引器
	indexerConfig.Embedding = embedder

	// 4. 创建最终可用的索引器实例
	idx, err := redisIndexer.NewIndexer(ctx, indexerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create indexer: %w", err)
	}

	return &RAGIndexer{
		embedding: embedder,
		indexer:   idx,
	}, nil
}

// IndexFile 读取文件内容并创建向量索引
func (r *RAGIndexer) IndexFile(ctx context.Context, filePath string) error {
	// 读取文件内容
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// 将文件内容转换为文档并进行文本切块
	docs := splitDocument(content, filePath)

	// 使用 indexer 存储文档（会自动进行向量化）
	_, err = r.indexer.Store(ctx, docs)
	if err != nil {
		return fmt.Errorf("failed to store document: %w", err)
	}

	return nil
}

// DeleteIndex 删除指定文件的知识库索引（静态方法，不依赖实例）
func DeleteIndex(ctx context.Context, filename string) error {
	if err := redisPkg.DeleteRedisIndex(ctx, filename); err != nil {
		return fmt.Errorf("failed to delete redis index: %w", err)
	}
	return nil
}
