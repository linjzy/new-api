# New API 部署

将本目录的脚本和说明安装到 `/opt/new-api/custom/`，脚本权限 `700`。
配置使用现有 `registry.env`（权限 `600`）；首次配置参考 [配置示例](registry.env.example)。
服务器只拉取预构建镜像，不持有 GitHub 令牌。

## 使用

```bash
/opt/new-api/custom/bin/newapi-custom-upgrade.sh status
/opt/new-api/custom/bin/newapi-custom-upgrade.sh upgrade latest   # 拉取 candidate 并部署
/opt/new-api/custom/bin/newapi-custom-upgrade.sh upgrade previous # 切回上一版 candidate
/opt/new-api/custom/bin/newapi-custom-upgrade.sh cleanup
```

`pull` 与 `deploy` 可拆成两步。部署通过 systemd 后台执行，flock 防止重叠：

1. 核对镜像；相同镜像且本机、公网健康时直接结束。
2. Compose 预检通过后，最多等待 120 秒自然空闲，再建立带任务归属的 nginx 维护门闩；验证本机 IPv4、IPv6 均返回带 `X-Newapi-Maintenance: 1` 的 503。支持排空协议的镜像通过容器内 `/new-api drain begin JOB_ID` 进入应用维护：空闲 Responses WS 发送 1012 关闭，忙连接等当前请求的结算和最终事件发送完成后关闭，已有 WS 的新请求返回 503。持续 Realtime 音频按完整会话等待，不主动中断。旧镜像只等待容器网络命名空间内 3000 端口的 TCP 连接归零，不执行新版 CLI。
3. 上游版本变化或尚无备份时，`pg_dump` 覆盖写入 `/opt/new-api/backups/new-api-before-upgrade.dump`，只保留这一份。
4. 上一版镜像本地标记为 `new-api-rollback:previous`，仅重建 `new-api`，健康后解除门闩并验证公网入口。
5. 失败自动切回本地上一版。切换前失败会取消本任务的应用排空，本机健康后恢复入口；替换容器后未经验证的失败保留维护门闩，回滚健康后才恢复。状态读取失败不视为排空，超时不强制关闭忙请求。成功后删除其余本地应用镜像。

恢复数据库时先停止应用：

```bash
cd /opt/new-api/deploy && docker compose stop new-api
docker exec -i new-api-postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" --clean --if-exists' < /opt/new-api/backups/new-api-before-upgrade.dump
docker compose start new-api
```

## 配置与排查

[nginx 配置](nginx-maintenance.conf)通过 `/run/newapi-maintenance` 拒绝新请求。
应用排空控制服务只监听容器内 `/tmp/newapi-drain/control.sock`，目录权限 700、socket 权限 600，不新增公网端口、HTTP 管理路由或业务凭据。脚本只对带 `com.linjzy.new-api.drain-protocol=1` 标签的容器执行 CLI；健康检查不计入业务计数。旧镜像通过 `nsenter` 检查实际容器内部端口，兼容 docker-proxy 和 NAT；调整应用内部端口时设置 `NEWAPI_APP_INTERNAL_PORT`。
`NEWAPI_IDLE_WAIT_SECONDS` 可设置自然空闲等待上限，设为 `0` 直接走门闩排空；`NEWAPI_DRAIN_WAIT_SECONDS` 仍控制门闩建立后的排空上限，超时不切换。自然空闲和门闩之间可能有新请求进入，因此两次检查缺一不可；该流程缩短拒绝新请求的窗口，但不保证没有 503。

`status` 显示运行镜像、保留的上一版、备份文件和任务结果；日志在 `logs/`，状态在 `state/`。
SIGTERM/INT/HUP 经过同一退出恢复逻辑。SIGKILL、主机中断或回滚失败留下的维护状态，可在核对任务已结束后执行 `newapi-custom-upgrade.sh recover`：只恢复有匹配任务归属记录的标记，确认应用健康并取消该任务的排空，再开放公网。其他任务的排空或手动维护标记不会被删除。
直接停止容器时，SIGTERM 会同时关闭应用准入并等待 HTTP 和已登记 WS；`SHUTDOWN_TIMEOUT_SECONDS` 缺省 120 秒，Compose 停止宽限为 180 秒。部署排空缺省等待 600 秒，期间尚未发送停止信号。
主 Compose 文件保持静态，`image` 指向 `candidate`；实际运行镜像由受管理的 override 钉到 digest，日常使用普通 `docker compose` 命令即可。
