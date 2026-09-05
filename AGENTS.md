# AGENTS.md

## Build Output

所有编译生成的可执行文件统一放到项目根目录下的 `bin/` 目录中。

- 本地构建: `make build` 生成 `bin/rysh`
- 交叉编译: `make cross` 生成 `dist/rysh-<os>-<arch>`
- 清理: `make clean` 删除 `bin/` 和 `dist/` 目录
