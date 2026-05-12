package router

import (
	"deeptalk/controller/image"

	"github.com/gin-gonic/gin"
)

func ImageRouter(r *gin.RouterGroup) {

	r.POST("/recognize", image.RecognizeImage)
}
