package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/smartcat999/container-ui/internal/service"
)

type ContainerHandler struct {
	dockerService *service.DockerService
}

func NewContainerHandler(dockerService *service.DockerService) *ContainerHandler {
	return &ContainerHandler{
		dockerService: dockerService,
	}
}

// GetContainers 获取容器列表
func (h *ContainerHandler) GetContainers(c *gin.Context) {
	contextName := c.Param("context")
	containers, err := h.dockerService.ListContainers(contextName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, containers)
}

// StartContainer 启动容器
func (h *ContainerHandler) StartContainer(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")
	err := h.dockerService.StartContainer(contextName, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Container started successfully"})
}

// StopContainer 停止容器
func (h *ContainerHandler) StopContainer(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")
	err := h.dockerService.StopContainer(contextName, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Container stopped successfully"})
}

// GetContainerDetail 获取容器详情
func (h *ContainerHandler) GetContainerDetail(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")
	detail, err := h.dockerService.GetContainerDetail(contextName, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

// GetContainerLogs 获取容器日志
func (h *ContainerHandler) GetContainerLogs(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")
	logs, err := h.dockerService.GetContainerLogs(contextName, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.String(http.StatusOK, logs)
}

// DeleteContainer 删除容器
func (h *ContainerHandler) DeleteContainer(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")
	force := c.Query("force") == "true"

	err := h.dockerService.DeleteContainer(contextName, id, force)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Container deleted successfully"})
}

// ListContainers 列出容器
func (h *ContainerHandler) ListContainers(c *gin.Context) {
	contextName := c.Param("context")
	containers, err := h.dockerService.ListContainers(contextName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, containers)
}

// ExecContainer 在容器中执行命令
func (h *ContainerHandler) ExecContainer(c *gin.Context) {
	contextName := c.Param("context")
	id := c.Param("id")

	// 升级HTTP连接为WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		HandshakeTimeout: 10 * time.Second,
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}
	defer ws.Close()

	// 创建执行配置
	execConfig := types.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          []string{"/bin/sh"},
		DetachKeys:   "ctrl-p,ctrl-q",
	}

	// 创建执行实例
	resp, err := h.dockerService.CreateExec(contextName, id, execConfig)
	if err != nil {
		log.Printf("Failed to create exec: %v", err)
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error creating exec: %v\n", err)))
		return
	}

	// 附加到执行实例
	hijackedResp, err := h.dockerService.AttachExec(contextName, resp.ID, execConfig.Tty)
	if err != nil {
		log.Printf("Failed to attach exec: %v", err)
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error attaching to exec: %v\n", err)))
		return
	}
	defer hijackedResp.Close()

	// 创建错误通道
	errChan := make(chan error, 2)

	// 启动数据转发
	go func() {
		buf := make([]byte, 1024)
		for {
			nr, err := hijackedResp.Read(buf)
			if err != nil {
				errChan <- err
				return
			}
			if nr > 0 {
				err := ws.WriteMessage(websocket.BinaryMessage, buf[:nr])
				if err != nil {
					errChan <- err
					return
				}
			}
		}
	}()

	go func() {
		for {
			messageType, p, err := ws.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			if messageType == websocket.TextMessage {
				var msg struct {
					Type string `json:"type"`
					Data string `json:"data"`
					Cols int    `json:"cols,omitempty"`
					Rows int    `json:"rows,omitempty"`
				}

				if err := json.Unmarshal(p, &msg); err != nil {
					continue
				}

				switch msg.Type {
				case "input":
					_, err = hijackedResp.Write([]byte(msg.Data))
					if err != nil {
						errChan <- err
						return
					}
				case "resize":
					if err := h.dockerService.ResizeExec(contextName, resp.ID, msg.Rows, msg.Cols); err != nil {
						log.Printf("Failed to resize terminal: %v", err)
					}
				}
			}
		}
	}()

	// 启动执行实例
	err = h.dockerService.StartExec(contextName, resp.ID, types.ExecStartCheck{
		Tty:    true,
		Detach: false,
	})
	if err != nil {
		log.Printf("Failed to start exec: %v", err)
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error starting exec: %v\n", err)))
		return
	}

	// 等待错误或连接关闭
	select {
	case err := <-errChan:
		if err != io.EOF {
			log.Printf("Connection error: %v", err)
		}
	case <-c.Done():
		log.Println("Client connection closed")
	}
}
