# 通知接收端容器（仿真件，不进交付镜像；交付镜像只有两个 Go 二进制）。
#
# 为什么要有这个镜像：Node-RED 跑在容器里，而接收端此前只有宿主机 venv 形态，
# 容器里的 `localhost` 指容器自己，投递会连到不存在的端口上（开放项 O-19）。
# 进 Compose 的 `sim` profile 后容器间走服务名 `wecom-mock:8081`，与接口规范
# 附录 B 的默认值一致，不用再改 `.env` 去猜宿主机地址。
#
# 镜像标签的唯一来源仍是 deploy/versions.env，这里是占位默认值。
ARG PYTHON_VERSION=3.12.7-slim-bookworm

FROM python:${PYTHON_VERSION}

# 非 root 运行：接收端要写记录库，但不需要任何特权。
RUN useradd --system --uid 1001 --create-home wecom

WORKDIR /app

# 先装依赖再拷代码：改代码不会重装 pip 层，构建缓存才用得上。
COPY requirements.txt ./
RUN pip install --no-cache-dir --disable-pip-version-check -r requirements.txt

COPY card.py store.py api.py main.py ./

# 记录库落 /data，由 compose 的卷挂出：容器重建后投递记录还在，
# 012 的「投递记录核对」要跨重启成立。
RUN mkdir -p /data && chown wecom /data
VOLUME ["/data"]

USER wecom

EXPOSE 8081

# --host 0.0.0.0 是容器内监听，宿主机侧仍由 compose 的 ports 限在 127.0.0.1，
# 不对外暴露（.claude/rules/sim.md）。宿主机直连调试走 `make sim-wecom`。
CMD ["python", "main.py", "--host", "0.0.0.0", "--db", "/data/wecom-mock.db"]
