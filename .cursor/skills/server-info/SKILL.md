---
name: server-info
description: Returns server connection information for known hosts and supports explicitly requested remote administration tasks. Use when the user asks for server access details, SSH connection commands, host connection information, or authorized server changes.
disable-model-invocation: true
---

# Server Info

## Instructions

Use the stored connection information for the requested server(s) only.

- If the user asks for server access details, provide the stored connection info exactly.
- Remote operations are allowed only when the user explicitly requests a concrete action on a known host.
- Do not run unrelated discovery, scanning, destructive operations, or broad system changes.
- Keep remote commands limited to the requested action.

## Oracle 01

| 项目 | 值 |
|------|-----|
| IP | 163.192.9.157 |
| SSH 端口 | 27312 |
| 用户 | ubuntu |
| 密码 | Pass@word!1234 |
| SSH 密钥 | `E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key` |
| 连接命令 | `ssh -i "E:/Files/SSH Key/oracle-ssh-key-2026-05-16.key" -p 27312 ubuntu@163.192.9.157` |
| 状态 | 服务器端已改为 27312；外部 SSH 登录已验证成功 |
