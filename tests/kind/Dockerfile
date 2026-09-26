FROM docker.io/python:3.12-alpine@sha256:c4634f578a412db396771b61b064c6e546c9d6414c7fb5b1b05d5871f1885f7b
ENV PYTHONDONTWRITEBYTECODE=1
COPY probe.py /app/probe.py
USER 65532:65532
ENTRYPOINT ["python3", "-B", "/app/probe.py"]
