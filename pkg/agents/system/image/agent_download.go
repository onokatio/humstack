package image

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func (a *ImageAgent) DownloadAPI(config *ImageAgentDownloadAPIConfig) error {
	r := gin.Default()

	corsConfig := cors.DefaultConfig()
	corsConfig.AllowOrigins = []string{"*"}
	r.Use(cors.New(corsConfig))

	r.GET("/api/v0/groups/:group_id/images/:image_id/tags/:tag/download", func(ctx *gin.Context) {
		groupID := ctx.Param("group_id")
		imageID := ctx.Param("image_id")
		tag := ctx.Param("tag")

		image, err := a.client.SystemV0().Image().Get(groupID, imageID)
		if err != nil {
			ctx.String(http.StatusInternalServerError, "%v", err)
			return
		}
		imageEntityID, ok := image.Spec.EntityMap[tag]
		if !ok {
			ctx.String(http.StatusNotFound, "notfound")
			return
		}

		ctx.Header("Content-Type", "application/octet-stream")
		ctx.Header("Content-Disposition", "attachment; filename= "+imageID)

		file, err := os.Open(filepath.Join(a.localImageDirectory, groupID, imageEntityID))
		if err != nil {
			ctx.String(http.StatusInternalServerError, "%v", err)
			return
		}
		defer file.Close()

		_, err = io.Copy(ctx.Writer, file)
		if err != nil {
			log.Println(err)
		}
	})

	if err := r.Run(fmt.Sprintf("%s:%d", config.AdvertiseAddress, config.ListenPort)); err != nil {
		return err
	}

	return nil
}