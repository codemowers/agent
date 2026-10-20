# Devices

- Device verbs are capabilities: `exec` for `device_shell`, `read` for
  `device_file_read` and `write` for `device_file_write`. `get` grants nothing.
  The bridge checks the verb on every call.
- `write` usually allows code execution by overwriting scripts or service
  files; grant it only where `exec` would be acceptable.
- Keep the generated key out of Git, chat, logs and shell history.
