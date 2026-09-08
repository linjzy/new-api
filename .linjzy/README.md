# New API 定制

基于 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 的已发布版本，
维护三份业务补丁。测试位于 `tests/`，发布脚本位于 `bin/`，部署工具位于 `deploy/`。

| 补丁 | 功能 |
| --- | --- |
| `usage-logs-auto-refresh.patch` | 用量日志自动刷新、错误去重，失败后停止刷新 |
| `sequential-key-mode.patch` | 多 Key 顺序切换、自动恢复和局部缓存更新 |
| `responses-capacity-retry.patch` | 输出前无计费用量的容量错误重试，保留已产生用量的结算 |

容量重试需要开启自动重试，并配置同模型、同分组的可用替代渠道。
已产生输出或计费用量的请求不会重放。

## 发布

[GitHub Actions](../.github/workflows/custom-image.yml) 每六小时运行，也支持 `main` 更新和手动触发。
流程按上游提交应用补丁，通过后端、数据库、前端及镜像启动检查后发布源码和镜像。
后端变更、定时任务或 `extended_checks=true` 会增加 MySQL 和 race 检查。

- 源码分支：`custom/<release>-custom-<build-sha>-<upstream-sha>`。
- 镜像标签：`ghcr.io/linjzy/new-api:<release>-custom-<build-sha>-<upstream-sha>`。
- 构建与验证分别计算摘要；已有镜像和匹配验证记录时复用结果。
- 定时发布或手动设置 `promote_candidate=true` 才更新 `candidate`；部署使用不可变 digest。
- 每次 Actions 构建前删除 `main` 以外的所有分支，构建后保留当前源码分支；复用镜像时按镜像记录的提交恢复分支。普通 PR 合并后由 GitHub 自动删分支。

补丁已验证适用于 rc.34、rc.35；中继结构改变时需重新移植，不自动合并不兼容代码。
服务器只拉取预构建镜像，操作见 [部署说明](deploy/README.md)。

## 本地验证

在指定上游提交的独立工作副本中放入 `.linjzy/`，从定制仓库运行：

```bash
bash .linjzy/bin/prepare-release.sh SOURCE RELEASE UPSTREAM_COMMIT SOURCE_REF
bash .linjzy/bin/verify-release.sh SOURCE all
bash .linjzy/bin/verify-release.sh SOURCE race
python3 -m unittest discover -s .linjzy/tests -p 'test_*.py'
actionlint .github/workflows/custom-image.yml
shellcheck .linjzy/bin/*.sh .linjzy/deploy/bin/*.sh
```

数据库验证设置 `CUSTOM_TEST_SQL_DRIVER=postgres|mysql` 和 `CUSTOM_TEST_SQL_DSN`，
再运行 `verify-release.sh SOURCE database`。测试会修改数据，只能使用临时测试库。
