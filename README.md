# onyxaxis2api

把 [ai.onyxaxis.org](https://ai.onyxaxis.org/) 的「生图实验室」包装成 OpenAI Images 兼容接口，暴露给任意 OpenAI 客户端调用。

上游没有开放的生图 API（站方 `/v1` 只稳定支持文本模型，生图模型走官方 API 会失败），所以本代理走**网页会话链路**：用登录后的浏览器会话 cookie 直接调用站点前端使用的内部接口，与「生图实验室」按钮背后的请求完全一致。

## 工作原理

```
下游客户端 ──OpenAI Images API──▶ onyxaxis2api ──cookie 会话──▶ ai.onyxaxis.org 网页后端
                                    │
                                    ├─ POST /api/images/generate        生图 / 改图（改图附 base64 参考图）
                                    ├─ GET  /api/attachments/{id}       结果图下载（响应未内联 b64 时兜底）
                                    └─ GET  /api/models                 生图模型列表（slug 映射）
```

- 生图一次约 30–90 秒，是同步长请求；代理默认上游超时 5 分钟。
- 上游成功响应**有时**直接带 `b64_json` 内联数据，**有时**只给附件地址；代理自动兜底下载附件再返回，下游永远拿到完整数据。

## 快速开始

```bash
# 1. 准备会话 cookie（每个账号一行，写入 accounts.txt）
#    在已登录 ai.onyxaxis.org 的浏览器里：
#    F12 → 网络(Network) → 随便点一个 /api 请求 → 请求标头 → 复制整条 cookie: 的值
cp accounts.example.txt accounts.txt
$EDITOR accounts.txt

# 2. 启动
PROXY_API_KEY=sk-your-own-key PORT=8090 ./onyxaxis2api

# 3. 调用
curl http://127.0.0.1:8090/v1/models -H "Authorization: Bearer sk-your-own-key"
```

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | `8090` | 监听端口 |
| `PROXY_API_KEY` | 必填 | 下游调用本代理用的 Bearer key |
| `ONYX_COOKIE` | - | 单账号 cookie（也可用文件） |
| `ONYX_ACCOUNTS_FILE` | `accounts.txt` | 多账号 cookie 文件，一行一个，轮询使用 |
| `ONYX_BASE_URL` | `https://ai.onyxaxis.org` | 上游地址 |
| `DEFAULT_MODEL` | `gpt-5.3` | 请求未指定 model 时的默认模型 |
| `MODEL_CACHE_TTL` | `5m` | 上游模型列表缓存时长 |
| `UPSTREAM_TIMEOUT` | `5m` | 上游单请求超时 |

## 接口

### GET /v1/models
列出上游当前可用的生图模型（`usable` 且 `supports_image_gen`）。

### POST /v1/images/generations —— 生图
```bash
curl -X POST http://127.0.0.1:8090/v1/images/generations \
  -H "Authorization: Bearer sk-your-own-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.3","prompt":"一只戴墨镜的橘猫在冲浪，卡通风格","n":1,"size":"1024x1024"}'
```

### POST /v1/images/edits —— 改图（参考图最多 5 张）
标准 multipart（OpenAI 客户端默认形式）：

```bash
curl -X POST http://127.0.0.1:8090/v1/images/edits \
  -H "Authorization: Bearer sk-your-own-key" \
  -F "model=gpt-5.3" \
  -F "prompt=把背景改成星空夜晚" \
  -F "image=@cat.png;type=image/png"
```

也接受 JSON（`image`/`images` 支持 data URL、裸 base64、http(s) 链接）：

```json
{"model":"gpt-5.3","prompt":"把背景改成星空夜晚","image":"data:image/png;base64,..."}
```

### 响应格式
默认返回 `b64_json`（内联数据，`gpt-image-1` 风格）。`"response_format":"url"` 时返回本代理的下载地址：
`GET /v1/images/file/{attachment_id}`（支持 Bearer、`X-Api-Key` 或 `?key=` 鉴权）。

### 参数说明
- `model`：`/v1/models` 里的 slug（如 `gpt-5.3`），也接受上游原始模型 id；写 `gpt-image-1` 之类写死的名字会自动落到默认模型。
- `size`：`1024x1024` / `1536x1024` / `1024x1536` / `1152x896` / `896x1152` / `1792x1024` / `1024x1792`，留空或 `auto` 为模型默认。
- `style`：留空 / `vivid` / `natural` / `anime` / `photographic` / `cinematic` / `digital-art` / `watercolor` / `oil-painting`（是否生效取决于模型）。

## 模型选择建议

| 模型 | 状态 |
| --- | --- |
| `gpt-5.3`（生图实验室里的 GPT 5.3） | **推荐**。生图与改图都验证可用，返回内联数据，账号内标为无限使用 |
| `gpt-image-2.5-flare`（GPT Image 2.5 Flare） | 上游供应商不稳定：经常返回链接导致站方拒绝中继，且消耗账号生图额度 |

## 注意事项

- **cookie 会过期**：上游返回 401 时代理会提示刷新 `accounts.txt`；在浏览器退出登录或站点清会话都会使其失效。
- **额度**：账号按 5 小时窗口计费（`/api/usage/me` 可查）；GPT 5.3 标记为无限使用，Flare 等计费模型失败也会白白消耗额度窗口，建议只用 `gpt-5.3`。
- **合规**：站方 API 条款禁止 SillyTavern/NSFW/角色扮演等用途，请遵守。
- `accounts.txt` 含登录凭证，已在 `.gitignore` 中排除，请勿提交。
