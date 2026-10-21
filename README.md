# SmartGate V3

面向 OpenWrt / ImmortalWrt 的代理管理面板，使用 Go + React，支持 sing-box 和 Xray。

- 订阅与节点管理、批量测速
- 业务池分流、规则管理与 DNS
- 连接查看、故障切换
- 配置预览、应用、备份与恢复

## 构建

需要 Go 1.26.1+、Node.js 22+、npm。默认构建 Linux ARM64 版本。

```sh
npm --prefix frontend ci
make test
make frontend-build
make build
make release
```

安装包：`dist/smartgate-openwrt-aarch64.tar.gz`。

安装与升级见 [部署文档](docs/RELEASE_RUNBOOK.md)，接口见 [API 文档](docs/API_CONTRACT.md)。管理面板仅供可信局域网使用。

[MIT License](LICENSE)
