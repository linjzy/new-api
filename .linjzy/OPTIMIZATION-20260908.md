# 2026-09-08 优化交付记录

本次完成三份业务补丁、发布验证流程及生产部署脚本的优化。不新增备份、历史镜像保留、
持久化上一版本或手动回退机制。原有当次失败自动恢复行为保留。

## 补丁

- Responses：上游已产生用量时，客户端首次写入前断连仍结算；无计费用量的失败保留退款与重试语义。
- Responses：请求层也延后设置 SSE 头和启动 ping，避免绕过流处理器的容量错误重试判定。
- 多 Key：禁用状态同步落库；通知异步执行，不阻塞下一把 Key。
- Key 恢复：只更新对应渠道和路由索引，保留轮询游标；渠道可用性改变时才刷新定价与任务路由。
- 日志刷新：请求层不重复弹出组件已负责展示的错误，失败停止自动刷新。

## 流程

- 镜像判重和候选 Compose 预检提前到维护之前；空载排空不再固定等待 10 秒。
- Compose 镜像引用只写受管理的 override；部署结束仅定向清理旧 New API 镜像。
- 显式清理限制在 New API 管理标签与仓库范围，移除全主机 Docker prune 和共享构建缓存清理。
- 统一生产脚本；旧日期入口仅转发；记录脚本 SHA、阶段、退出码及耗时。
- 分离构建身份和验证身份，先检查镜像与匹配的验证记录。仓库认证、网络错误会停止流程，不当作缓存未命中。
- 恢复计费、Key、通知阻塞、自动刷新回归门禁；PostgreSQL 为常规门禁，相关变更增加 MySQL 与 race 检查。
- 前端 lint、类型检查和测试在发布前执行，Dockerfile 负责生产构建。

## 发布前已验证

- 三份最终补丁逐个直接应用 rc.34、rc.35；rc.35 的 42 个补丁目标文件与测试源码一致。
- Go 针对性门禁：relay、relay/channel、openai、model、service 通过。
- Key 恢复、容量错误与通知阻塞的 race 检查通过。
- 独立 PostgreSQL 15.19 数据库的恢复、过期结果拒绝及并发恢复测试通过；测试数据库已停止并清除。
- 前端类型检查、受影响文件 lint、生产构建通过；3 个前端测试通过。
- Playwright 实际组件交互验证 HTTP 和业务错误各只显示一个提示，并停止刷新。
- 8 个部署/构建身份/镜像查询契约测试通过；ShellCheck、actionlint、差异空白检查通过。
- 生产脚本原子安装，无备份文件；完整 systemd 同镜像升级返回 0，约 1 秒，阶段为 unchanged。
- 生产本机和公网状态 success=true，应用仍 rc.34；应用、PostgreSQL、Redis 启动时间不变。

生产脚本 SHA-256：
`8ff44477e6c5bfdd8096b37811949f5a9f08e53f21eb716ba2749f9b7876c94a`

## 发布与部署结果

2026-09-08 11:29:27（Asia/Shanghai）完成生产部署，应用由 rc.34 升级至带本次优化补丁的 rc.35。

- 优化提交：[`1a3fa18`](https://github.com/linjzy/new-api/commit/1a3fa18c07906330a06f5386f0464e1bd4fa0606)。
- [完整发布流水线](https://github.com/linjzy/new-api/actions/runs/34182780328)成功：Go、SQLite、PostgreSQL、MySQL、race、8 个 Python 契约测试、前端 lint/类型检查/3 个回归、Docker 生产构建及容器启动检查全部通过。
- [候选更新流水线](https://github.com/linjzy/new-api/actions/runs/34183269808)成功：命中同镜像与完整验证记录，跳过源码准备、依赖安装、测试和构建，更新 candidate。
- 公开源码：[`6657a420`](https://github.com/linjzy/new-api/tree/6657a420507e310b661830df485e083ef9e6eefe)，上游提交 `bee45b58a3c0b77e8dc81e6b5aeb4474aa9058d1`。
- 部署镜像：`ghcr.io/linjzy/new-api@sha256:2b599a8afa8a5772af6f2bfc5a58de2b5d836c10224150773452882b4cd8f998`；线上标签中的补丁 SHA 与本地三份最终补丁一致。
- systemd 部署任务 `newapi-custom-activate-20260908032851.service` 返回 0，阶段 complete，总耗时 36 秒，其中已有连接排空 27 秒，激活与健康验证 8 秒。
- 本机和公网 `/api/status` 均为 HTTP 200、success=true、版本 v1.0.0-rc.35；公网首页与引用的 JavaScript 资源为 HTTP 200。
- 新应用容器 healthy，启动日志确认 PostgreSQL、Redis 及 rc.35；PostgreSQL/Redis 容器 ID、镜像和启动时间保持不变。
- candidate、验证记录与生产镜像 digest 一致；维护门闩已解除，临时候选和调度状态已清理。
- 旧 rc.34 镜像和当次临时回退 tag 已删除；服务器只剩运行中的 New API、PostgreSQL、Redis 三个镜像，备份文件数量为 0。

## 未来版本兼容范围

当前未发布的 fork main 与 rc.34/rc.35 的中继结构不同；已验证预检会在修改应用源码前拒绝不兼容的结构。
新旧前端路径可显式识别，但该新中继结构仍需针对未来发布单独移植，不能自动强套补丁。

入口：[补丁与 CI 说明](README.md)、[部署说明](deploy/README.md)。
