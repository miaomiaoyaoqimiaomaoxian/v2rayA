# v2rayA v2.5.7-per-source.1 · Windows x64

这是基于官方 v2rayA v2.5.7 的 fork 构建，不是官方原版安装包。此 Release 提供一个可安装的 Windows x64 Inno Setup 程序，包含补丁版 `v2raya.exe`、匹配的 v2.5.7 核心、Wintun 和 geodata。

## 为什么需要这个补丁

SSSDOG 的订阅链接只有约 5 分钟有效期。链接过期后再进行全局定时更新，可能让该订阅下的节点全部失效。单一全局开关无法同时满足“其他订阅定时更新”和“SSSDOG 永不自动更新”。

## 主要改动

- 每个订阅源分别设置“不自动更新”“服务启动时更新”或“按小时定时更新”；间隔按各自最近一次更新尝试时间计算。关闭自动更新不影响手动更新。
- 旧数据库首次升级时迁移原全局设置；已有每源设置原样保留。定时器只处理到期的订阅源，并用稳定的数据库 ID 定位，避免订阅排序变动导致更新错源。
- 新版订阅编辑窗口提供独立策略入口，订阅卡片显示当前策略；旧的全局更新控件移除，更新时采用的代理模式设置保留。
- 核心更新失败也记录尝试时间，避免失效链接在短间隔内反复请求。

涉及 `service/db/schema.go`、`service/db/subscription_policy.go`、`service/db/listOp.go`、`service/pre_update.go`、`service/server/service/subscription*.go`、`service/kernel/touch/touch.go`、`gui/src/dialogs/Subscription.vue`、`gui/src/dialogs/SubscriptionSettings.vue` 和订阅页面/卡片等文件。安装包由 `install/windows-inno/package-custom.ps1` 按官方 Inno 模板生成，所需公开资源提取自经过 SHA-256 核验的官方 v2.5.7 安装包。

## 验证与边界

- 前端 213 项测试、TypeScript 类型检查、国际化键检查和每源策略的 Go 专项测试通过。
- 安装包解包后，管理程序、核心、Wintun 和 geodata 与打包输入逐文件比对哈希通过。
- 已在一台 Windows x64 机器上运行此管理程序和核心，Web/数据库配置正常；本 Release 的安装程序未在该机器覆盖安装运行，以避免改变现有服务与数据。安装前请自行备份 v2rayA 程序及数据。
- 安装程序未进行代码签名，Windows 可能提示未知发布者。TUN 与具体节点联网效果不由本次静态包校验保证。
