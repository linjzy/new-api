# New API 定制

基于 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 的已发布版本维护三份业务补丁。
补丁在 `patches/`，测试在 `tests/`，发布脚本在 `bin/`，服务器部署工具在 `deploy/`。

| 补丁 | 功能 |
| --- | --- |
| `usage-logs-auto-refresh.patch` | 用量日志自动刷新、错误去重，失败后停止刷新 |
| `sequential-key-mode.patch` | 多 Key 顺序切换、自动恢复和局部缓存更新 |
| `responses-capacity-retry.patch` | 输出前无计费用量的容量错误重试，保留已产生用量的结算 |

容量重试需要开启自动重试，并配置同模型、同分组的可用替代渠道。
已产生输出或计费用量的请求不会重放。

## 发布

[GitHub Actions](../.github/workflows/custom-image.yml) 每六小时运行；`.linjzy/` 下非文档改动推送到 `main`，或手动触发时也运行。
流程：取最新上游 release，在其提交上应用补丁，通过后端、数据库、前端和镜像启动检查后发布源码分支和镜像。

- 镜像 `ghcr.io/linjzy/new-api:<release>-custom-<build-sha>-<upstream-sha>`，源码分支 `custom/<同名>`；分支不含 `.github/workflows`。
- 构建输入（补丁、prepare 脚本）与验证输入（tests、deploy、verify 脚本）分别计算摘要；镜像和匹配的 `verified-*` 记录已存在时直接复用，改 workflow 或文档不会重建。
- 定时运行或手动 `promote_candidate=true` 才移动 `candidate`；定时运行和后端补丁变更会追加 MySQL 与 race 检查。
- 每次成功后只保留 `candidate`、本次镜像和最近一个其他镜像及其 `custom/*` 分支，其余 ghcr 版本和分支删除。

补丁失配时构建失败，日志打印 `.rej` 内容。移植：

```bash
git fetch https://github.com/QuantumNous/new-api.git "refs/tags/$TAG" && git worktree add ../port FETCH_HEAD
cp -R .linjzy ../port/ && cd ../port
bash .linjzy/bin/prepare-release.sh . "$TAG" "$(git rev-parse HEAD)" custom/port   # 失败时保留部分应用结果和 .rej
```

修好后重新生成对应补丁并提交到本仓库。补丁已验证适用于 rc.34、rc.35。
`main` 的其余内容是上游 tag 的快照，只为方便阅读；需要时 `git merge <tag>` 同步。

## 本地验证

```bash
bash .linjzy/bin/verify-release.sh SOURCE all
bash .linjzy/bin/verify-release.sh SOURCE race
python3 -m unittest discover -s .linjzy/tests -p 'test_*.py'
actionlint .github/workflows/custom-image.yml
shellcheck .linjzy/bin/*.sh .linjzy/deploy/bin/*.sh
```

数据库验证设置 `CUSTOM_TEST_SQL_DRIVER=postgres|mysql` 和 `CUSTOM_TEST_SQL_DSN`，
再运行 `verify-release.sh SOURCE database`。测试会修改数据，只能使用临时测试库。
