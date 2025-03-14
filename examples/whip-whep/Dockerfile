FROM ubuntu:latest
LABEL authors="ubuntu"

RUN apt update && apt install -y ca-certificates && apt install curl lsof wget -y
WORKDIR /app
COPY ./whip-whep /app/whip-whep
COPY ./index.html /app/index.html

ENTRYPOINT ["/app/whip-whep"]