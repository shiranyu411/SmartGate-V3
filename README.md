# SmartGate V3

面向 OpenWrt / ImmortalWrt 的代理管理面板，使用 Go + React，支持 sing-box 和 Xray。

- 订阅与节点管理、批量测速
- 业务池分流、规则管理与 DNS
- 连接查看、故障切换
- 配置预览、应用、备份与恢复

## 构建

需要 Go 1.26.1+、Node.js 22+、npm；测试另需 Python 3 和 ripgrep。默认构建 Linux ARM64 版本。

```sh
npm --prefix frontend ci
make test
make frontend-build
make build
make release
```

安装包：`dist/smartgate-openwrt-aarch64.tar.gz`。

安装与升级见 [部署文档](docs/RELEASE_RUNBOOK.md)，接口见 [API 文档](docs/API_CONTRACT.md)。管理面板仅供可信局域网使用。

## 许可证与署名

SmartGate V3 的原创项目代码采用 [Apache License 2.0](LICENSE) 授权。原项目作者及维护者：[@shiranyu411](https://github.com/shiranyu411)。

再分发源代码、安装包或修改后的衍生作品时，应遵守 Apache-2.0 的版权声明、变更说明及 [NOTICE](NOTICE) 署名保留要求。第三方素材与依赖仍遵循各自许可证；仅私下运行软件不要求公开署名。

