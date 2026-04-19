# Console Manager

轻量级 Web 终端管理器，快速创建和管理多个独立的交互式终端实例。

## 功能特性

- 🖥️ **多实例管理** - 创建和管理多个控制台实例，支持独立的工作目录和环境变量
- 📡 **实时终端交互** - 基于 WebSocket 的 PTY（伪终端）实时交互
- 🌐 **现代 Web 界面** - 内置 xterm.js 终端模拟器，开箱即用
- 🔄 **完整生命周期** - 启动、停止、重启进程管理
- 🔑 **身份验证** - Web 界面登录保护，首次自动生成凭证
- 💾 **实例持久化** - 所有实例配置自动保存，支持自启动功能
- 📜 **历史缓冲** - 256KB 环形缓冲区保存终端输出历史
- 📐 **自适应尺寸** - 实时跟踪终端宽高变化
- 🚀 **单文件部署** - 前端内嵌，仅需一个二进制文件
- 🆓 **随机端口** - 首次运行自动分配端口，之后固定使用
- ⏹️ **优雅关闭** - 停止时自动优雅停止所有运行中的实例

## 快速开始（直接使用）

### 1. 从 Release 下载二进制文件

访问 [Releases 页面](https://github.com/SanVenturas/console-manager/releases)，下载对应系统架构的可执行文件：
- `console-manager-linux-amd64` - Linux x86-64
- `console-manager-linux-arm64` - Linux ARM64（树莓派等）


### 2. 启动应用

使用项目中的启动脚本进行管理：

下载启动脚本：https://github.com/SanVenturas/console-manager/blob/main/start.sh

启动脚本用法

```bash
./start.sh start       # 启动应用（后台运行）
./start.sh stop        # 停止应用
./start.sh restart     # 重启应用
./start.sh status      # 查看运行状态
./start.sh log         # 实时查看日志

# 自定义端口启动
PORT=9090 ./start.sh start
```

首次运行会：
- 自动选择随机可用端口
- 生成随机用户名密码（保存到 `.credentials`）
- 创建默认配置文件

### 3. 访问 Web 界面

打开浏览器访问显示的 URL（如 `http://localhost:8080`），输入首次启动时生成的用户名和密码。


## 本地开发

### 前置要求

- Go 1.21+
- GNU Make

### 拉取代码

```bash
git clone https://github.com/SanVenturas/console-manager.git
cd console-manager
```

### 构建

编译为本地平台的可执行文件：

```bash
make build
# 输出文件: ./console-manager
```

或使用 Go 直接构建：

```bash
cd backend
go build -o ../console-manager .
```

### 跨平台构建

构建所有支持平台的可执行文件：

```bash
# Linux x86-64
GOOS=linux GOARCH=amd64 go build -o ../console-manager-linux-amd64 ./backend

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -o ../console-manager-linux-arm64 ./backend
```

### 开发运行

```bash
cd backend
go run main.go
```

应用会在随机端口启动，查看控制台输出查找访问 URL。

### 项目结构

```
.
├── backend/              # Go 后端代码
│   ├── main.go          # 主程序，包含 HTTP 服务、WebSocket、PTY 管理
│   ├── go.mod           # Go 模块定义
│   └── static/          # 前端静态文件（内嵌）
│       └── index.html   # Web UI
├── deploy/              # 部署相关文件
│   └── console-manager.service  # systemd 服务文件
├── .github/workflows/   # GitHub Actions 配置
│   └── release.yml      # 自动构建和发布工作流
├── start.sh             # 启动脚本
├── Dockerfile           # Docker 镜像配置
├── Makefile             # 构建配置
└── README.md            # 自述文件
```

## 技术栈

- **后端**: Go 1.21 (stdlib + gorilla/websocket + creack/pty)
- **前端**: 原生 HTML/JavaScript + xterm.js 5.5.0
- **部署**: 单二进制文件 + systemd 或启动脚本
- **CI/CD**: GitHub Actions（自动多架构构建）
