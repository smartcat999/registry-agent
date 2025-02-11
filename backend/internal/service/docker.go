package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/volume"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type DockerService struct {
	clients map[string]*client.Client // 存储多个 context 的 client
}

type ContainerInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	State   string `json:"state"`
	Created int64  `json:"created"`
	Ports   []Port `json:"ports"`
}

type Port struct {
	IP          string `json:"ip"`
	PrivatePort uint16 `json:"privatePort"`
	PublicPort  uint16 `json:"publicPort"`
	Type        string `json:"type"`
}

type ImageInfo struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       int64  `json:"size"`
	Created    int64  `json:"created"`
}

type NetworkInfo struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Driver  string       `json:"driver"`
	Scope   string       `json:"scope"`
	IPAM    network.IPAM `json:"ipam"`
	Created time.Time    `json:"created"`
}

type VolumeInfo struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	CreatedAt  string            `json:"created"`
	Labels     map[string]string `json:"labels"`
	Scope      string            `json:"scope"`
	Options    map[string]string `json:"options"`
}

// ContextConfig 定义
type ContextConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // tcp or socket
	Host    string `json:"host"` // tcp://host:port 或 unix:///path/to/socket
	Current bool   `json:"current"`
}

// 构建 Docker Host URL
func buildDockerHost(config ContextConfig) string {
	return config.Host
}

// 解析 Docker Host URL
func parseDockerHost(hostURL string) (string, int, string) {
	if strings.HasPrefix(hostURL, "tcp://") {
		host := strings.TrimPrefix(hostURL, "tcp://")
		parts := strings.Split(host, ":")
		if len(parts) == 2 {
			port, _ := strconv.Atoi(parts[1])
			return parts[0], port, ""
		}
		return host, 2375, ""
	}
	return "", 0, strings.TrimPrefix(hostURL, "unix://")
}

// ContainerConfig 容器配置
type ContainerConfig struct {
	ImageID       string
	Name          string
	Command       string
	Args          []string
	Ports         []PortMapping
	Env           []EnvVar
	Volumes       []VolumeMapping
	RestartPolicy string
	NetworkMode   string
}

// PortMapping 端口映射
type PortMapping struct {
	Host      uint16
	Container uint16
}

// EnvVar 环境变量
type EnvVar struct {
	Key   string
	Value string
}

// VolumeMapping 数据卷映射
type VolumeMapping struct {
	Host      string
	Container string
	Mode      string
}

const (
	configDir  = ".docker-contexts"
	configFile = "contexts.json"
)

// 获取配置文件路径
func getConfigPath() string {
	dir := filepath.Join(".", configDir)

	// 确保配置目录存在
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			// 如果无法创建目录，使用当前目录
			return filepath.Join(".", configFile)
		}
	}

	return filepath.Join(dir, configFile)
}

// 读取配置
func readConfig() (map[string]interface{}, error) {
	configPath := getConfigPath()

	// 如果文件不存在，创建默认配置
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := map[string]interface{}{
			"contexts":        make(map[string]interface{}),
			"current-context": "",
		}
		data, err := json.MarshalIndent(defaultConfig, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(configPath, data, 0644); err != nil {
			return nil, err
		}
		return defaultConfig, nil
	}

	// 读取现有配置
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return config, nil
}

// 保存配置
func saveConfig(config map[string]interface{}) error {
	configPath := getConfigPath()

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

func NewDockerService() (*DockerService, error) {
	return &DockerService{
		clients: make(map[string]*client.Client),
	}, nil
}

// getClient 根据 context name 获取或创建对应的 Docker client
func (s *DockerService) getClient(contextName string) (*client.Client, error) {
	// 检查是否已有该 context 的 client
	if cli, exists := s.clients[contextName]; exists {
		return cli, nil
	}

	// 读取 context 配置
	config, err := readConfig()
	if err != nil {
		return nil, err
	}

	contexts, ok := config["contexts"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no contexts found")
	}

	contextConfig, ok := contexts[contextName].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("context %s not found", contextName)
	}

	host, _ := contextConfig["host"].(string)
	if host == "" {
		return nil, fmt.Errorf("invalid host configuration for context %s", contextName)
	}

	// 创建新的 client
	cli, err := client.NewClientWithOpts(
		client.WithHost(host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %v", err)
	}

	// 保存 client
	s.clients[contextName] = cli
	return cli, nil
}

func (s *DockerService) ListContainers(contextName string) ([]ContainerInfo, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return nil, err
	}

	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var containerInfos []ContainerInfo
	for _, container := range containers {
		// 处理容器名称，移除开头的 "/"
		name := strings.TrimPrefix(container.Names[0], "/")

		// 转换端口信息
		var ports []Port
		for _, p := range container.Ports {
			ports = append(ports, Port{
				IP:          p.IP,
				PrivatePort: p.PrivatePort,
				PublicPort:  p.PublicPort,
				Type:        p.Type,
			})
		}

		containerInfos = append(containerInfos, ContainerInfo{
			ID:      container.ID[:12], // 只显示ID的前12位
			Name:    name,
			Image:   container.Image,
			Status:  container.Status,
			State:   container.State,
			Created: container.Created,
			Ports:   ports,
		})
	}

	return containerInfos, nil
}

func (s *DockerService) StartContainer(contextName string, id string) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	return cli.ContainerStart(context.Background(), id, types.ContainerStartOptions{})
}

func (s *DockerService) StopContainer(contextName string, id string) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	return cli.ContainerStop(context.Background(), id, container.StopOptions{})
}

func (s *DockerService) GetContainerDetail(contextName string, id string) (types.ContainerJSON, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return types.ContainerJSON{}, err
	}
	return cli.ContainerInspect(context.Background(), id)
}

func (s *DockerService) ListImages(contextName string) ([]ImageInfo, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return nil, err
	}

	images, err := cli.ImageList(context.Background(), types.ImageListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var imageInfos []ImageInfo
	for _, image := range images {
		// 处理 RepoTags，可能为空
		repository := "<none>"
		tag := "<none>"
		if len(image.RepoTags) > 0 {
			parts := strings.Split(image.RepoTags[0], ":")
			if len(parts) == 2 {
				repository = parts[0]
				tag = parts[1]
			}
		}

		imageInfos = append(imageInfos, ImageInfo{
			ID:         image.ID[7:19], // 移除 "sha256:" 前缀并截取
			Repository: repository,
			Tag:        tag,
			Size:       image.Size,
			Created:    image.Created,
		})
	}

	return imageInfos, nil
}

func (s *DockerService) DeleteImage(contextName string, id string) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	_, err = cli.ImageRemove(context.Background(), id, types.ImageRemoveOptions{Force: false})
	return err
}

func (s *DockerService) CreateContainer(contextName string, config ContainerConfig) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}

	// 准备端口绑定
	portBindings := nat.PortMap{}
	exposedPorts := nat.PortSet{}
	if len(config.Ports) > 0 {
		for _, p := range config.Ports {
			containerPort := nat.Port(fmt.Sprintf("%d/tcp", p.Container))
			hostBinding := nat.PortBinding{
				HostIP:   "0.0.0.0",
				HostPort: fmt.Sprintf("%d", p.Host),
			}
			portBindings[containerPort] = []nat.PortBinding{hostBinding}
			exposedPorts[containerPort] = struct{}{}
		}
	}

	// 准备环境变量
	var env []string
	if len(config.Env) > 0 {
		env = make([]string, len(config.Env))
		for i, e := range config.Env {
			env[i] = fmt.Sprintf("%s=%s", e.Key, e.Value)
		}
	}

	// 准备数据卷
	var binds []string
	if len(config.Volumes) > 0 {
		binds = make([]string, len(config.Volumes))
		for i, v := range config.Volumes {
			binds[i] = fmt.Sprintf("%s:%s:%s", v.Host, v.Container, v.Mode)
		}
	}

	// 准备重启策略
	var restartPolicy container.RestartPolicy
	switch config.RestartPolicy {
	case "always":
		restartPolicy = container.RestartPolicy{Name: "always"}
	case "unless-stopped":
		restartPolicy = container.RestartPolicy{Name: "unless-stopped"}
	case "on-failure":
		restartPolicy = container.RestartPolicy{Name: "on-failure"}
	default:
		restartPolicy = container.RestartPolicy{Name: "no"}
	}

	// 准备命令和参数
	var cmd []string
	if config.Command != "" {
		cmd = append(cmd, config.Command)
		if len(config.Args) > 0 {
			cmd = append(cmd, config.Args...)
		}
	}

	// 创建容器配置
	containerConfig := &container.Config{
		Image: config.ImageID,
	}

	// 只有在有命令时才设置
	if len(cmd) > 0 {
		containerConfig.Cmd = cmd
	}

	// 只有在有端口时才设置
	if len(exposedPorts) > 0 {
		containerConfig.ExposedPorts = exposedPorts
	}

	// 只有在有环境变量时才设置
	if len(env) > 0 {
		containerConfig.Env = env
	}

	// 主机配置
	hostConfig := &container.HostConfig{
		RestartPolicy: restartPolicy,
	}

	// 只有在有端口映射时才设置
	if len(portBindings) > 0 {
		hostConfig.PortBindings = portBindings
	}

	// 只有在有数据卷时才设置
	if len(binds) > 0 {
		hostConfig.Binds = binds
	}

	// 只有在指定网络模式时才设置
	if config.NetworkMode != "" {
		hostConfig.NetworkMode = container.NetworkMode(config.NetworkMode)
	}

	// 创建容器
	resp, err := cli.ContainerCreate(
		context.Background(),
		containerConfig,
		hostConfig,
		nil,         // 网络配置，使用默认值
		nil,         // 平台配置，使用默认值
		config.Name, // 如果名称为空，Docker 会自动生成
	)
	if err != nil {
		return fmt.Errorf("failed to create container: %v", err)
	}

	// 启动容器
	if err := cli.ContainerStart(context.Background(), resp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	}

	return nil
}

func (s *DockerService) GetImageDetail(contextName string, id string) (types.ImageInspect, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return types.ImageInspect{}, err
	}
	inspect, _, err := cli.ImageInspectWithRaw(context.Background(), id)
	if err != nil {
		return types.ImageInspect{}, err
	}
	return inspect, nil
}

func (s *DockerService) ListNetworks(contextName string) ([]NetworkInfo, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return nil, err
	}

	networks, err := cli.NetworkList(context.Background(), types.NetworkListOptions{})
	if err != nil {
		return nil, err
	}

	var networkInfos []NetworkInfo
	for _, network := range networks {
		networkInfos = append(networkInfos, NetworkInfo{
			ID:      network.ID,
			Name:    network.Name,
			Driver:  network.Driver,
			Scope:   network.Scope,
			IPAM:    network.IPAM,
			Created: network.Created,
		})
	}

	return networkInfos, nil
}

func (s *DockerService) GetNetworkDetail(contextName string, id string) (types.NetworkResource, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return types.NetworkResource{}, err
	}
	return cli.NetworkInspect(context.Background(), id, types.NetworkInspectOptions{})
}

func (s *DockerService) DeleteNetwork(contextName string, id string) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	return cli.NetworkRemove(context.Background(), id)
}

func (s *DockerService) ListVolumes(contextName string) ([]VolumeInfo, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return nil, err
	}

	volumes, err := cli.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		return nil, err
	}

	var volumeInfos []VolumeInfo
	for _, volume := range volumes.Volumes {
		volumeInfos = append(volumeInfos, VolumeInfo{
			Name:       volume.Name,
			Driver:     volume.Driver,
			Mountpoint: volume.Mountpoint,
			CreatedAt:  volume.CreatedAt,
			Labels:     volume.Labels,
			Scope:      volume.Scope,
			Options:    volume.Options,
		})
	}

	return volumeInfos, nil
}

func (s *DockerService) GetVolumeDetail(contextName string, name string) (volume.Volume, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return volume.Volume{}, err
	}
	return cli.VolumeInspect(context.Background(), name)
}

func (s *DockerService) DeleteVolume(contextName string, name string) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	return cli.VolumeRemove(context.Background(), name, true)
}

func (s *DockerService) GetContainerLogs(contextName string, id string) (string, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return "", err
	}

	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Tail:       "1000", // 获取最后1000行日志
	}

	logs, err := cli.ContainerLogs(context.Background(), id, options)
	if err != nil {
		return "", err
	}
	defer logs.Close()

	// 读取日志内容
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(logs)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (s *DockerService) ListContexts() ([]ContextConfig, error) {
	config, err := readConfig()
	if err != nil {
		return nil, err
	}

	currentCtx, _ := config["current-context"].(string)
	contextsMap, ok := config["contexts"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no contexts found")
	}

	var contextConfigs []ContextConfig
	var currentConfig *ContextConfig

	for name, ctx := range contextsMap {
		contextConfig, ok := ctx.(map[string]interface{})
		if !ok {
			continue
		}

		contextType, _ := contextConfig["type"].(string)
		host, _ := contextConfig["host"].(string)

		config := ContextConfig{
			Name:    name,
			Type:    contextType,
			Host:    host,
			Current: name == currentCtx,
		}

		if name == currentCtx {
			currentConfig = &config
		} else {
			contextConfigs = append(contextConfigs, config)
		}
	}

	// 按名称排序非当前上下文
	sort.Slice(contextConfigs, func(i, j int) bool {
		return contextConfigs[i].Name < contextConfigs[j].Name
	})

	// 将当前上下文插入到列表开头
	if currentConfig != nil {
		contextConfigs = append([]ContextConfig{*currentConfig}, contextConfigs...)
	}

	return contextConfigs, nil
}

func (s *DockerService) CreateContext(config ContextConfig) error {
	// 创建 context 时不再自动切换和创建 client
	currentConfig, err := readConfig()
	if err != nil {
		return err
	}

	contexts, ok := currentConfig["contexts"].(map[string]interface{})
	if !ok {
		contexts = make(map[string]interface{})
		currentConfig["contexts"] = contexts
	}

	contexts[config.Name] = map[string]interface{}{
		"type": config.Type,
		"host": config.Host,
	}

	return saveConfig(currentConfig)
}

func (s *DockerService) DeleteContext(name string) error {
	config, err := readConfig()
	if err != nil {
		return err
	}

	// 检查是否为当前使用的上下文
	if currentContext, ok := config["current-context"].(string); ok && currentContext == name {
		return fmt.Errorf("cannot delete current context: %s", name)
	}

	contexts, ok := config["contexts"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("no contexts found")
	}

	if _, exists := contexts[name]; !exists {
		return fmt.Errorf("context %s not found", name)
	}

	delete(contexts, name)
	return saveConfig(config)
}

func (s *DockerService) GetContextConfig(name string) (string, error) {
	config, err := readConfig()
	if err != nil {
		return "", err
	}

	contexts, ok := config["contexts"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no contexts found")
	}

	contextConfig, ok := contexts[name].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("context %s not found", name)
	}

	host, ok := contextConfig["host"].(string)
	if !ok {
		return "", fmt.Errorf("invalid host configuration for context %s", name)
	}

	return host, nil
}

func (s *DockerService) UpdateContextConfig(name string, config ContextConfig) error {
	currentConfig, err := readConfig()
	if err != nil {
		return err
	}

	contexts, ok := currentConfig["contexts"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("no contexts found")
	}

	if _, exists := contexts[name]; !exists {
		return fmt.Errorf("context %s not found", name)
	}

	// 更新配置
	contexts[name] = map[string]interface{}{
		"type": config.Type,
		"host": config.Host,
	}

	// 如果是当前上下文，更新 Docker 客户端
	if currentContext, ok := currentConfig["current-context"].(string); ok && currentContext == name {
		dockerHost := buildDockerHost(config)
		os.Setenv("DOCKER_HOST", dockerHost)

		cli, err := client.NewClientWithOpts(
			client.FromEnv,
			client.WithAPIVersionNegotiation(),
		)
		if err != nil {
			return fmt.Errorf("failed to create docker client: %v", err)
		}
		s.clients[name] = cli
	}

	return saveConfig(currentConfig)
}

func (s *DockerService) DeleteContainer(contextName string, id string, force bool) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	options := types.ContainerRemoveOptions{
		Force:         force, // 如果容器正在运行，是否强制删除
		RemoveVolumes: false, // 默认不删除关联的匿名卷
	}
	return cli.ContainerRemove(context.Background(), id, options)
}

// CreateExec 创建执行实例
func (s *DockerService) CreateExec(contextName string, containerID string, config types.ExecConfig) (types.IDResponse, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return types.IDResponse{}, err
	}
	return cli.ContainerExecCreate(context.Background(), containerID, config)
}

// AttachExec 附加到执行实例
func (s *DockerService) AttachExec(contextName string, execID string, tty bool) (io.ReadWriteCloser, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return nil, err
	}
	resp, err := cli.ContainerExecAttach(context.Background(), execID, types.ExecStartCheck{
		Tty:    tty,
		Detach: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to attach exec: %v", err)
	}
	return resp.Conn, nil
}

// StartExec 启动执行实例
func (s *DockerService) StartExec(contextName string, execID string, config types.ExecStartCheck) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	err = cli.ContainerExecStart(context.Background(), execID, config)
	if err != nil {
		return fmt.Errorf("failed to start exec: %v", err)
	}
	return nil
}

// ResizeExec 调整终端大小
func (s *DockerService) ResizeExec(contextName string, execID string, height, width int) error {
	cli, err := s.getClient(contextName)
	if err != nil {
		return err
	}
	return cli.ContainerExecResize(context.Background(), execID, types.ResizeOptions{
		Height: uint(height),
		Width:  uint(width),
	})
}

// GetServerInfo 获取服务器信息
func (s *DockerService) GetServerInfo(contextName string) (types.Info, error) {
	cli, err := s.getClient(contextName)
	if err != nil {
		return types.Info{}, fmt.Errorf("failed to get docker client: %v", err)
	}

	info, err := cli.Info(context.Background())
	if err != nil {
		return types.Info{}, fmt.Errorf("failed to get server info: %v", err)
	}

	return info, nil
}
