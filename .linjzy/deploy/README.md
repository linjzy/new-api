# New API 部署

将本目录的脚本和说明安装到 `/opt/new-api/custom/`，脚本权限设为 `700`。
配置使用现有 `registry.env`（权限 `600`）；首次配置参考 [配置示例](registry.env.example)。
更新文件使用原子替换，不生成备份。服务器只拉取预构建镜像，GitHub Actions 不持有服务器凭据。
自动清理分支需将仅授权此仓库 `Actions: write` 的令牌存入权限为 `600` 的私有文件，
在 `registry.env` 设置 `NEWAPI_GITHUB_TOKEN_FILE` 指向它；不配置则关闭分支清理。

## 使用

```bash
/opt/new-api/custom/bin/newapi-custom-upgrade.sh status
/opt/new-api/custom/bin/newapi-custom-upgrade.sh pull latest
/opt/new-api/custom/bin/newapi-custom-upgrade.sh deploy
/opt/new-api/custom/bin/newapi-custom-upgrade.sh upgrade latest
/opt/new-api/custom/bin/newapi-custom-upgrade.sh cleanup
/opt/new-api/custom/bin/newapi-custom-upgrade.sh cleanup-branches
```

`pull` 拉取并锁定镜像 digest；`deploy` 部署候选；`upgrade` 完成两步。
部署通过 systemd 后台执行，并使用 flock 防止重叠。

1. 核对镜像标签；相同镜像且本机、公网健康时直接结束。
2. Compose 预检通过后建立 nginx 维护门闩，等待已有连接结束；空载立即继续。
3. 更新受管理的 Compose override，仅重建 `new-api`。
4. 核对镜像和本机健康，解除维护后验证公网状态与前端入口。
5. 成功后立即删除旧应用镜像和临时回退标签。
6. 触发 GitHub 分支清理，保留生产和候选源码。`cleanup-branches` 可对健康的当前版本单独触发。

沿用当次失败自动恢复；不保留历史镜像、上一版本状态或配置、数据库备份。
`cleanup` 仅处理 New API 管理标签和仓库范围内的资源，不清理其他项目、数据库卷或共享构建缓存。

## 配置与排查

[nginx 配置](nginx-maintenance.conf)通过 `/run/newapi-maintenance` 拒绝新请求。
排空检测适用于 `127.0.0.1:8899 -> docker-proxy -> new-api:3000`；调整端口或取消 docker-proxy 时需同步修改。

`status` 显示运行镜像、候选和任务结果；日志位于 `logs/`，当前版本与任务阶段位于 `state/`。
`state/branch-cleanup.json` 保存清理任务 ID；任务未结束前拒绝下一次应用切换。触发失败不会回退健康应用，结果不明确时需先在 Actions 核实，不自动重复提交。
主 Compose 文件保持静态，日常使用普通 `docker compose` 命令以包含 override。
容量重试、计费、Key 恢复和自动刷新由发布前回归验证；部署健康检查不发送计费请求。
