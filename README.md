# Console Manager

轻量级控制台管理器

## 功能

- 🖥️ 创建和管理多个控制台实例
- 📡 基于 WebSocket 的实时终端交互（PTY）
- 🌐 现代 Web UI（xterm.js）
- 🔄 进程启动/停止/重启
- 📜 输出历史缓冲（256KB 环形缓冲区）
- 📐 终端尺寸自适应
- 🚀 单二进制部署（前端内嵌）

## 构建

```bash
make build
```

### 方式一：启动脚本（推荐快速部署）

使用 `nohup` 后台运行，关闭 SSH 后服务不中断：

```bash
cd /opt/console-manager
./start.sh start      # 启动（后台运行）
./start.sh stop       # 停止
./start.sh restart    # 重启
./start.sh status     # 查看状态
./start.sh log        # 查看实时日志

# 自定义端口
PORT=9090 ./start.sh start
```

### 方式二：systemd 服务（推荐生产环境）

开机自启 + 自动重启：

```bash
scp deploy/console-manager.service user@server:/etc/systemd/system/
ssh user@server "systemctl enable --now console-manager"

# 管理命令
systemctl status console-manager
systemctl restart console-manager
journalctl -u console-manager -f
```

## API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/consoles` | 列出所有控制台 |
| POST | `/api/consoles` | 创建控制台 |
| GET | `/api/consoles/{id}` | 获取控制台详情 |
| DELETE | `/api/consoles/{id}` | 删除控制台 |
| POST | `/api/consoles/{id}/start` | 启动控制台 |
| POST | `/api/consoles/{id}/stop` | 停止控制台 |
| WS | `/ws/{id}` | WebSocket 终端连接 |

## 创建控制台示例

```bash
curl -X POST http://localhost:8080/api/consoles \
  -H "Content-Type: application/json" \
  -d '{"name":"My Shell","command":"/bin/bash","args":[],"autoStart":true}'
```

## 技术栈

- **后端**: Go (stdlib + gorilla/websocket + creack/pty)
- **前端**: 原生 HTML/JS + xterm.js
- **部署**: 单二进制 + systemd
