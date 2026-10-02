package v2

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) StartRvtoolsCollector(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{"error": "not available in this build"})
}
