# New API 部署流程

权威实现位于本仓库 `.linjzy/deploy/`。服务器只运行预构建镜像，不克隆应用源码、
应用补丁或执行 Docker 构建。GitHub Actions 不持有生产服务器凭据。

## 安装与配置

将 `bin/newapi-custom-upgrade.sh` 和本说明同步到 `/opt/new-api/custom/` 对应路径，
脚本权限设为 `700`。原子替换目标文件，不生成备份副本。
保留服务器现有 `registry.env`；首次安装可参考 `registry.env.example`，权限设为 `600`。
`NEWAPI_*` 参数统一写在该配置文件中，后台 systemd 任务会读取相同路径。

`nginx-maintenance.conf` 在代理到应用的 location 中检查 `/run/newapi-maintenance`。
该文件存在时拒绝新请求，已有响应继续执行。既有 nginx 配置无需在每次升级时 reload。
连接计数适用于当前 `127.0.0.1:8899 -> docker-proxy -> new-api:3000` 拓扑；
如果更换发布端口或取消 docker-proxy，应先调整排空检测，不能把空的宿主 socket 统计当成请求已结束。

## 命令与执行顺序

```bash
/opt/new-api/custom/bin/newapi-custom-upgrade.sh status
/opt/new-api/custom/bin/newapi-custom-upgrade.sh pull latest
/opt/new-api/custom/bin/newapi-custom-upgrade.sh deploy
/opt/new-api/custom/bin/newapi-custom-upgrade.sh upgrade latest
/opt/new-api/custom/bin/newapi-custom-upgrade.sh cleanup
```

`pull` 解析并保存不可变 digest；`deploy` 使用已拉取候选；`upgrade` 先拉取再部署。
后两者通过 systemd transient service 后台执行，并用 flock 防止任务重叠。

1. 拉取镜像并核对平台、仓库、公开源码和补丁标签。
2. 比较候选与运行镜像。镜像相同且本机、公网健康时直接结束，不维护、不重建、不清理。
3. 在临时输入中渲染候选 Compose 配置并预检，失败不改线上配置。
4. 建立 nginx 维护门闩并验证 503；等待已有连接结束。空载立即通过；有连接时每秒检查，
   最多等待配置的排空时限。连接检查失败会停止部署。
5. 仅写受管理的 `docker-compose.override.yml`。主 Compose 文件保持静态，
   镜像引用以 override 为准；使用普通 `docker compose` 命令以包含它。
6. `docker compose up -d --no-deps --pull never new-api`，仅重建应用。
7. 检查本机状态、实际 image ID，解除维护后检查公网 `/api/status` 和前端 HTML 入口。
8. 记录当前版本和任务结果；成功后立即定向删除已不使用的 New API 镜像。

前端入口检查不代表已验证所有业务接口；容量重试、计费、Key 恢复和自动刷新由发布前回归门禁覆盖。
生产部署不主动发送计费请求。

## 不保留备份

沿用当次部署失败时自动恢复原镜像的既有行为。成功后立即删除临时回退 tag 和旧镜像，
不新增持久化上一版本状态、不保留历史镜像、不创建配置或数据库备份。

## 清理边界

正常升级只定向删除旧的 New API 镜像。其余维护通过独立 `cleanup` 命令执行：

- 容器、网络、匿名卷必须带 `com.linjzy.new-api.managed=true` 才进入清理范围。
- 运行镜像和待部署候选受保护；不使用全主机 `image prune --all`。
- 不清理其他 Compose 项目、不清除共享 BuildKit 缓存、不删除 PostgreSQL/Redis 数据卷。
- 仅清理 New API 工具自身的过期日志和已知旧构建文件。

## 状态与日志

`state/current.env` 记录当前镜像、上游版本、补丁和源码信息；`state/candidate.env`
记录已拉取候选；`state/job.env` 记录最后任务阶段、脚本 SHA-256、退出码和耗时。
这些文件只存运行元数据，不是可恢复备份。

任务日志位于 `logs/`。`status` 同时显示容器、候选、当前版本、后台任务和脚本版本。
日期命名的旧入口如需兼容，仅转发到同一个权威脚本，不维护独立实现。
