# officeace2api 反代（回环 39055）
proxy_pass http://127.0.0.1:39055;
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# SSE：必须关掉缓冲，否则流式对话会被整段攒到最后才吐
proxy_buffering off;
proxy_cache off;

# 长连接：对话可能跑很久，图片生成实测 10-30s
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;

# 图像 edits 可能带 base64 大 body
client_max_body_size 32m;
