FROM python:3.13-slim-bookworm
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin imageapp
WORKDIR /app
COPY --chown=10001:10001 app.py passwords.py secret_store.py /app/
USER 10001:10001
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
CMD ["python", "-I", "/app/app.py"]
