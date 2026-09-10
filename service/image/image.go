package image

import (
	"context"
	"deeptalk/common/image"
	"fmt"
	"io"
	"log"
	"mime/multipart"
)

// MaxImageBytes 单张图片大小上限：识别前先校验，避免把大图整个读进内存再 base64（内存会膨胀约 1.4 倍）
const MaxImageBytes = 8 << 20 // 8MB

func RecognizeImage(ctx context.Context, file *multipart.FileHeader) (string, error) {
	if file == nil {
		return "", fmt.Errorf("empty file")
	}
	if file.Size > MaxImageBytes {
		return "", fmt.Errorf("image too large: %d bytes (max %d)", file.Size, MaxImageBytes)
	}

	// 创建识别器（自动读取 config.toml 中的阿里云 API 配置）
	recognizer, err := image.NewImageRecognizer()
	if err != nil {
		log.Println("NewImageRecognizer fail err is : ", err)
		return "", err
	}
	defer recognizer.Close()

	src, err := file.Open()
	if err != nil {
		log.Println("file open fail err is : ", err)
		return "", err
	}
	defer src.Close()

	// 用 LimitReader 兜底：file.Size 不可信时也不会读爆内存
	buf, err := io.ReadAll(io.LimitReader(src, MaxImageBytes+1))
	if err != nil {
		log.Println("io.ReadAll fail err is : ", err)
		return "", err
	}
	if len(buf) > MaxImageBytes {
		return "", fmt.Errorf("image too large after read: %d bytes", len(buf))
	}

	return recognizer.PredictFromBuffer(ctx, buf)
}
