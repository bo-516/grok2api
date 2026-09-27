package server

// mediaDoc is the image and video section of /doc.
// It must not contain the letters g-r-o-k in order: the guide test rejects that
// word so callers are not told which runtime is behind the endpoint.
// {{origin}} is replaced by usageDoc before this text is appended.
// The bodies match what the server accepts: image n, size, edits, and the video
// submit/poll shape. A missing limit here is how callers send a field the server rejects.
const mediaDoc = `
6. 图片

对话消息里不能带图片。出图用下面两个地址，请求头与对话相同。
一次大约十几秒到一分钟。客户端超时请至少 3 分钟，并且不要自动重试。
启动时若关闭了媒体，这两个地址和下面的视频地址都返回 404，code 为 media_disabled。

文生图：
POST {{origin}}/v1/images/generations
Content-Type: application/json

{"prompt":"一只橘猫坐在窗台上，水彩","size":"1792x1024","n":1}

n 只能是 1 到 4。size 可用 256x256、512x512、1024x1024、1792x1024、1024x1792、1536x1024、1024x1536、auto。
也可以写 aspect_ratio：1:1、16:9、9:16、3:2、2:3、auto。不要和 size 同时写。
4:3 会按 3:2，3:4 按 2:3，更宽的横向比例按 16:9，更高的竖向比例按 9:16，并出现在响应头 X-Agent-Mock-Ignored。
response_format 为 url（默认，指向 /v1/media/）或 b64_json。

200：
{"created":1790178012,"data":[{"url":"{{origin}}/v1/media/<32位十六进制>.jpg","revised_prompt":"一只橘猫坐在窗台上，水彩"}],"output_format":"jpeg"}

GET 那个 url 即可下载，不需要 API key。文件默认保留 1 小时，进程重启后失效。

改图：
POST {{origin}}/v1/images/edits
multipart 字段 image，多张时字段名为 image[]，最多 5 张。prompt 必填。
也可以发 JSON：

{"prompt":"改成铅笔素描","image":{"url":"{{origin}}/v1/media/<上一步的文件名>.jpg"}}

多张写 images：{"prompt":"...","images":[{"url":"..."},{"url":"..."}]}
图片来源：上传的文件、data:image/png、data:image/jpeg、data:image/webp 的 base64、本服务的 /v1/media 链接，或 https 链接。每张不超过 20 MiB。
http 链接、file_id、mask 会返回 400。

可以写但不会改变画面，并出现在 X-Agent-Mock-Ignored：model、quality、style、background、output_compression、resolution、user。

7. 视频

POST {{origin}}/v1/videos/generations
Content-Type: application/json

用一张已有的图做首帧：

{"prompt":"镜头缓慢推进","image":{"url":"{{origin}}/v1/media/<文件名>.jpg"},"duration":6,"aspect_ratio":"16:9","resolution":"480p"}

只有文字、没有图片时，同一次任务里会先出首帧再出视频：

{"prompt":"海边的灯塔，镜头缓缓拉远","duration":6,"aspect_ratio":"16:9"}

立刻返回 {"request_id":"<uuid>"}。然后每隔几秒：

GET {{origin}}/v1/videos/<request_id>

排队或生成中：
{"status":"pending","progress":0}

progress 为 0 表示在排队，10 表示正在出首帧（仅纯文字视频），50 表示已经开始出视频。

完成：
{"status":"done","progress":100,"video":{"url":"{{origin}}/v1/media/<32位十六进制>.mp4","duration":6,"respect_moderation":true}}

失败：
{"status":"failed","error":{"code":"internal_error","message":"..."}}

未知或过期的 id 返回 404，code 为 video_not_found。

duration 为 1 到 15，不写则是 8。resolution 为 480p（默认）或 720p。1080p 按 720p 生成，并出现在 X-Agent-Mock-Ignored。
aspect_ratio 可用 1:1、16:9、9:16、4:3、3:4、3:2、2:3。给了 png 或 jpeg 首帧、又没写比例时，按图片宽高取最接近的一项；否则默认 16:9。
prompt 最长 4000 字。给了 image 时可以不写 prompt。
image 是首帧。reference_images 最多 4 张。keyframes 最多 4 个，每项是 {"image":{"url":"..."},"timestamp_s":3}，时间必须大于 0 且小于 duration。
reference_audios 最多 3 个，只接受 {"voice_id":"<音色 id>"}，不接受 url。
model、output、storage_options、user 会被接受但不会改变成片。状态里的 model 是你提交的值；没写时不要把状态里的 model 拿去当对话模型。

视频常常要几分钟。同时排队加运行的任务数有上限，超出返回 429。产物和任务记录默认保留 1 小时。

下载图片或视频：
GET {{origin}}/v1/media/<文件名>
不需要 API key，支持 Range。
`
