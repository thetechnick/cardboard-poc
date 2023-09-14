FROM scratch

COPY /bin/cardboard /
ENTRYPOINT ["/cardboard"]
