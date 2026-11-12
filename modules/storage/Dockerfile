FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends lvm2 util-linux udev ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY diskfleet /app/diskfleet
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
EXPOSE 8080
CMD ["python", "-m", "diskfleet.api"]
