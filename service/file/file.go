package file

import (
	"context"
	cachepkg "deeptalk/internal/cache"
	"deeptalk/internal/rag"
	"deeptalk/utils"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
)

// 上传rag相关文件（这里只允许文本文件）
// 其实可以直接将其向量化进行保存，但这边依旧存储到服务器上以便后续可以在服务器上查看历史RAG文件
func UploadRagFile(username string, file *multipart.FileHeader) (string, error) {
	// 校验文件类型和文件名
	if err := utils.ValidateFile(file); err != nil {
		log.Printf("File validation failed: %v", err)
		return "", err
	}

	// 创建用户目录
	userDir := filepath.Join("uploads", username)
	if err := os.MkdirAll(userDir, 0755); err != nil {
		log.Printf("Failed to create user directory %s: %v", userDir, err)
		return "", err
	}

	// 删除用户目录中的所有现有文件及其索引（每个用户只能有一个文件）
	files, err := os.ReadDir(userDir)
	if err == nil {
		for _, f := range files {
			if !f.IsDir() {
				filename := f.Name()
				// 删除该文件对应的 Redis 索引
				if err := rag.DeleteIndex(context.Background(), filename); err != nil {
					log.Printf("Failed to delete index for %s: %v", filename, err)
					// 继续执行，不因为索引删除失败而中断文件上传
				}
			}
		}
	}
	// 删除用户目录中的所有文件
	if err := utils.RemoveAllFilesInDir(userDir); err != nil {
		log.Printf("Failed to clean user directory %s: %v", userDir, err)
		return "", err
	}

	// 生成UUID作为唯一文件名
	uuid := utils.GenerateUUID()

	ext := filepath.Ext(file.Filename)
	filename := uuid + ext
	filePath := filepath.Join(userDir, filename)

	// 打开上传的文件
	src, err := file.Open()
	if err != nil {
		log.Printf("Failed to open uploaded file: %v", err)
		return "", err
	}
	defer src.Close()

	// 创建目标文件
	dst, err := os.Create(filePath)
	if err != nil {
		log.Printf("Failed to create destination file %s: %v", filePath, err)
		return "", err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		log.Printf("Failed to copy file content: %v", err)
		return "", err
	}

	log.Printf("File uploaded successfully: %s", filePath)

	// 创建 RAG 索引器并对文件进行向量化
	indexer, err := rag.NewRAGIndexer(filename)
	if err != nil {
		log.Printf("Failed to create RAG indexer: %v", err)
		// 删除已上传的文件
		os.Remove(filePath)
		return "", err
	}

	// 读取文件内容并创建向量索引
	if err := indexer.IndexFile(context.Background(), filePath); err != nil {
		log.Printf("Failed to index file: %v", err)
		// 删除已上传的文件和索引
		os.Remove(filePath)
		rag.DeleteIndex(context.Background(), filename)
		return "", err
	}

	// 索引名由文件名决定（rag_docs:<文件名>:idx），换了文件就是换了索引。
	// RAGQuery 是按用户名缓存的，这里必须主动失效，否则会一直查旧索引 —— 属于
	// 静默返回错误答案的那种问题，比报错难查得多。
	rag.InvalidateUser(username)

	// 答案缓存同样要失效：旧文档的答案已经不对，而且此前"文档里没有答案"
	// 的拒答判断也不再成立。语义缓存按向量近邻查，无法精确知道哪些答案受
	// 这篇文档影响，因此整体清空——文档更新是低频操作，宁多花几次模型调用，
	// 也不能返回基于旧文档的答案。
	cachepkg.InvalidateAll()

	log.Printf("File indexed successfully: %s", filename)
	return filePath, nil
}
