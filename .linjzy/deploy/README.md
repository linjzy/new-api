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
2. Compose 预检通过后，最多等待 120 秒的自然空闲窗口（等待时公网继续服务），再建立 nginx 维护门闩并再次排空连接。等不到自然空闲则回退原有门闩排空流程；连接检查失败或结果无效时终止，不执行切换。回滚仍直接使用门闩排空。
3. 上游版本变化或尚无备份时，`pg_dump` 覆盖写入 `/opt/new-api/backups/new-api-before-upgrade.dump`，只保留这一份。
4. 上一版镜像本地标记为 `new-api-rollback:previous`，仅重建 `new-api`，健康后解除门闩并验证公网入口。
5. 失败自动切回本地上一版；成功后删除其余本地应用镜像。

恢复数据库时先停止应用：

```bash
cd /opt/new-api/deploy && docker compose stop new-api
docker exec -i new-api-postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" --clean --if-exists' < /opt/new-api/backups/new-api-before-upgrade.dump
docker compose start new-api
```

## 配置与排查

[nginx 配置](nginx-maintenance.conf)通过 `/run/newapi-maintenance` 拒绝新请求。
排空检测适用于 `127.0.0.1:8899 -> docker-proxy -> new-api:3000`；调整端口或取消 docker-proxy 时需同步修改。
`NEWAPI_IDLE_WAIT_SECONDS` 可设置自然空闲等待上限，设为 `0` 直接走门闩排空；`NEWAPI_DRAIN_WAIT_SECONDS` 仍控制门闩建立后的排空上限，超时不切换。自然空闲和门闩之间可能有新请求进入，因此两次检查缺一不可；该流程缩短拒绝新请求的窗口，但不保证没有 503。

`status` 显示运行镜像、保留的上一版、备份文件和任务结果；日志在 `logs/`，状态在 `state/`。
主 Compose 文件保持静态，`image` 指向 `candidate`；实际运行镜像由受管理的 override 钉到 digest，日常使用普通 `docker compose` 命令即可。
