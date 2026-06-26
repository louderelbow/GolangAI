package image

import (
	"deeptalk/common/image"
	"io"
	"log"
	"mime/multipart"
)

func RecognizeImage(file *multipart.FileHeader) (string, error) {
	// 创建识别器（自动读取 config.toml 中的阿里云 API 配置）
	recognizer, err := image.NewImageRecognizer("", "", 0, 0)
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

	buf, err := io.ReadAll(src)
	if err != nil {
		log.Println("io.ReadAll fail err is : ", err)
		return "", err
	}

	return recognizer.PredictFromBuffer(buf)
}
