package ccconnect

import "embed"

// SetupAssets 随二进制分发，用户安装后不必再下载源码才能打开配置向导。
//
//go:embed tools/feishu-collab-setup.py tools/collab_config.py
var SetupAssets embed.FS
