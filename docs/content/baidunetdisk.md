---
title: "Baidu Netdisk"
description: "Rclone docs for Baidu Netdisk"
versionIntroduced: "v1.76"
---

# Baidu Netdisk

[Baidu Netdisk](https://pan.baidu.com/) (百度网盘) is a Chinese consumer
cloud storage service by Baidu. It stores files under the `baidunetdisk`
alias.

Paths are specified as `remote:path`.

Paths may be as deep as required, e.g. `remote:directory/subdirectory`.

## Configuration

Here is an example of how to make a remote called `remote`.  First run:

```console
rclone config
```

This will guide you through an interactive setup process:

```text
n) New remote
r) Rename remote
c) Copy remote
s) Set configuration password
q) Quit config
n/r/c/s/q> n
name> remote
Option Storage.
Type of storage to configure.
Choose a number from below, or type in your own value.
...
XX / Baidu Netdisk
   \ "baidunetdisk"
Storage> baidunetdisk
Option refresh_token.
The refresh token used to mint access tokens. See the OpenList wiki for how to obtain one.
Enter a value. Press Enter to leave empty.
refresh_token> <your refresh token>
```

The `refresh_token` is minted by logging in to Baidu's OAuth flow once. The
[OpenList wiki](https://github.com/alist-org/alist) documents how to obtain
it. The backend exchanges it for a short-lived `access_token` on demand,
and when Baidu rotates the refresh token the new value is written back
into the config file.

The refresh runs through exactly one of two paths, selected with
`use_online_api` (default `false`). The choice is validated when the remote
is created, and the missing option fails with an explicit error:

- `use_online_api = true`: the access token is minted by the endpoint in
  `api_url_address`. This is a token refresh proxy hosted by the OpenList
  project; there is no default and you must supply your own endpoint to use
  this mode. No Baidu client credentials are needed.
- `use_online_api = false` (default): the access token is minted directly
  from Baidu's OAuth endpoint, which requires `client_id` and
  `client_secret`.

### Download APIs

Baidu restricts download links, so the backend resolves them through one of
three mechanisms, selected with the `download_api` option:

- `official` (default): the `/xpan/multimedia` file metadata with
  `dlink=1`, plus the access token in the query string. The CDN answers the
  first HEAD with a redirect, which the backend chases without following,
  and every ranged read is streamed over the resolved URL. This API needs
  an active account in good standing.
- `crack`: the `/api/filemetas` endpoint with a `dlna` origin, which returns
  a direct download link without the redirect chase. This works on accounts
  that are not "active" enough for the official API.
- `crack_video`: the `/api/mediainfo` endpoint, the best choice for video
  files. It carries the same `custom_crack_ua` user agent as `crack`.

The `crack` and `crack_video` requests are stamped with the
`custom_crack_ua` user agent (default `netdisk`); the official downloads use
`pan.baidu.com`.

## Downloads

Files are read as a series of ranged requests over one resolved download
URL per file, which the backend re-signs only when it expires. The API and
metadata traffic use the default HTTP client, while the download requests
run on a dedicated client stamped with the user agent each download API
expects.

## Uploads

Baidu requires the md5 of every slice before the first byte can be uploaded,
so the backend hashes the source in a single pass, then uploads the slices.
The source is read exactly once; remote sources are never downloaded twice.

Sources that are not local files are spooled to a local temp file while
hashing, and the slices upload from the spool. When the source is a local
file there is no spool at all: the stream is hashed while discarding it and
the slices upload by re-opening ranges of the local file.

When the source can provide an md5 without reading the stream, the server is
first asked whether the content already exists: a hit creates the entry
instantly and skips the transfer entirely (秒传). Uploads of content the
server already has also skip reading the source in multi-thread copies.

Because the slice hashes gate the whole upload, this backend splits one
transfer's progress 50/50: source reads fill the first half of the bar and
the uploaded slices fill the second, so the bar stays alive through the
upload instead of freezing at 100%. The upload itself is also logged every 5%
so it stays visible.

Empty files cannot be uploaded: Baidu refuses them server-side.

The slice size follows the account tier (regular 4 MiB, VIP 16 MiB, super VIP
32 MiB, at most 2048 slices), fetched lazily on the first upload. Slices
upload in parallel (`upload_thread`, default 3); an expired uploadid restarts
the upload from scratch automatically.

## Modification times

Baidu has no API to set a file's modification time; the timestamps you see
on files are assigned by the server and may differ from the source. Because
of this the backend reports that modification times are not supported, and
comparisons between Baidu Netdisk and other remotes are made by size. Use
`--size-only` when correctness of the timestamps matters less than avoiding
re-copies, e.g.

```console
rclone copy /local/path remote:path --size-only
```

Note that a file whose content changed without changing its size is not
detected as changed by a size-based comparison.

## Standard options

Here follows the backend options for Baidu Netdisk.

<!-- autogenerated options start - DO NOT EDIT - instead edit fs.RegInfo in backend/baidunetdisk/baidunetdisk.go and run make backenddocs to verify --> <!-- markdownlint-disable-line line-length -->
### Standard options

Here are the Standard options specific to baidunetdisk (Baidu Netdisk).

#### --baidunetdisk-refresh-token

The refresh token used to mint access tokens. See the OpenList wiki for how to obtain one.

Properties:

- Config:      refresh_token
- Env Var:     RCLONE_BAIDUNETDISK_REFRESH_TOKEN
- Type:        string
- Required:    true

#### --baidunetdisk-download-api

Which download API to use to resolve download URLs.

Properties:

- Config:      download_api
- Env Var:     RCLONE_BAIDUNETDISK_DOWNLOAD_API
- Type:        string
- Default:     "official"
- Examples:
  - "official"
    - official: multimedia dlink with access_token, chases the CDN redirect (needs an active account)
  - "crack"
    - crack: /api/filemetas dlna dlink, works on non-active accounts
  - "crack_video"
    - crack_video: /api/mediainfo dlink, best for video files

#### --baidunetdisk-use-online-api

Whether to refresh the access token through the online api_url_address endpoint (true) or through the direct OAuth flow with client_id/client_secret (false). When true, api_url_address is required; when false, client_id and client_secret are required.

Properties:

- Config:      use_online_api
- Env Var:     RCLONE_BAIDUNETDISK_USE_ONLINE_API
- Type:        bool
- Default:     false

#### --baidunetdisk-api-url-address

Online API address used when use_online_api is true. This is a token refresh proxy provided by the OpenList project; supply your own if you use this mode.

Properties:

- Config:      api_url_address
- Env Var:     RCLONE_BAIDUNETDISK_API_URL_ADDRESS
- Type:        string
- Required:    false

#### --baidunetdisk-client-id

Baidu OAuth client_id, required when use_online_api is false.

Properties:

- Config:      client_id
- Env Var:     RCLONE_BAIDUNETDISK_CLIENT_ID
- Type:        string
- Required:    false

#### --baidunetdisk-client-secret

Baidu OAuth client_secret, required when use_online_api is false.

Properties:

- Config:      client_secret
- Env Var:     RCLONE_BAIDUNETDISK_CLIENT_SECRET
- Type:        string
- Required:    false

#### --baidunetdisk-custom-crack-ua

User-Agent used for the crack and crack_video download APIs.

Properties:

- Config:      custom_crack_ua
- Env Var:     RCLONE_BAIDUNETDISK_CUSTOM_CRACK_UA
- Type:        string
- Default:     "netdisk"

#### --baidunetdisk-upload-api

Upload API domain base. Used as fallback when use_dynamic_upload_api is true and the dynamic lookup fails.

Properties:

- Config:      upload_api
- Env Var:     RCLONE_BAIDUNETDISK_UPLOAD_API
- Type:        string
- Default:     "https://d.pcs.baidu.com"

#### --baidunetdisk-use-dynamic-upload-api

Dynamically fetch the upload domain via locateupload before uploading. The upload_api setting is used as fallback when the lookup fails.

Properties:

- Config:      use_dynamic_upload_api
- Env Var:     RCLONE_BAIDUNETDISK_USE_DYNAMIC_UPLOAD_API
- Type:        bool
- Default:     true

#### --baidunetdisk-upload-thread

Number of parallel slices uploaded at once (1-32).

Properties:

- Config:      upload_thread
- Env Var:     RCLONE_BAIDUNETDISK_UPLOAD_THREAD
- Type:        int
- Default:     3

#### --baidunetdisk-upload-timeout

Per-slice upload timeout in seconds.

Properties:

- Config:      upload_timeout
- Env Var:     RCLONE_BAIDUNETDISK_UPLOAD_TIMEOUT
- Type:        int
- Default:     60

### Advanced options

Here are the Advanced options specific to baidunetdisk (Baidu Netdisk).

#### --baidunetdisk-custom-upload-part-size

Custom upload slice size in bytes, 0 for automatic. Only applies to VIP accounts.

Properties:

- Config:      custom_upload_part_size
- Env Var:     RCLONE_BAIDUNETDISK_CUSTOM_UPLOAD_PART_SIZE
- Type:        int
- Default:     0

#### --baidunetdisk-low-bandwith-upload-mode

Grow the slice size in 1 MiB steps until the slice count fits MaxSliceNum (for low bandwidth).

Properties:

- Config:      low_bandwith_upload_mode
- Env Var:     RCLONE_BAIDUNETDISK_LOW_BANDWITH_UPLOAD_MODE
- Type:        bool
- Default:     false

#### --baidunetdisk-encoding

The encoding for the backend. See the encoding section of the docs for more info.

Properties:

- Config:      encoding
- Env Var:     RCLONE_BAIDUNETDISK_ENCODING
- Type:        Encoding
- Default:     Slash,Del,Ctl,InvalidUtf8,Dot

#### --baidunetdisk-description

Description of the remote.

Properties:

- Config:      description
- Env Var:     RCLONE_BAIDUNETDISK_DESCRIPTION
- Type:        string
- Required:    false

<!-- autogenerated options stop -->
