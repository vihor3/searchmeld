# 上游更新策略

SearchMeld 是独立维护的项目，不自动同步或整体合并 One Search 上游分支。

本地 remote 约定：

- `origin`：`vihor3/searchmeld`，SearchMeld 的发布与协作仓库。
- `upstream`：`CncCbz/one-search`，只用于查看可选更新。
- `legacy-fork`：`vihor3/one-search`，仅保留既有上游 PR 和历史兼容用途。

选择性引入上游更新时：

```bash
git fetch upstream
git log --oneline --reverse main..upstream/main
git show <upstream-commit>
git switch -c upstream/<topic> main
git cherry-pick -x <upstream-commit>
```

每次引入都应通过独立分支和 PR 完成，并运行完整的后端、前端、安装脚本及容器配置测试。`cherry-pick -x` 会在提交信息中保留来源提交，便于后续审计和继续同步。

不执行以下操作：

- 不把 `upstream/main` 直接 merge 到 `main`。
- 不为上游创建自动同步工作流。
- 不覆盖 SearchMeld 的兼容接口、数据库迁移、安装方式或品牌标识。
