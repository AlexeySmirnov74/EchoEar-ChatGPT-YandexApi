FROM python:3.13-slim-bookworm
RUN pip install --no-cache-dir 'Pillow==13.0.0' \
 && useradd --uid 10001 --create-home --shell /usr/sbin/nologin imageapp
WORKDIR /app
COPY --chown=10001:10001 yandex_image_search.py frame_worker.py decoder_service.py service.py secret_store.py /app/
USER 10001:10001
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
CMD ["python", "-I", "/app/service.py"]
