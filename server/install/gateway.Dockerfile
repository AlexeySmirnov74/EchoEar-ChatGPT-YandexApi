FROM node:24-alpine
WORKDIR /app
COPY gateway/gateway-alarm-clock.js /app/gateway.js
COPY gateway/admin-proxy.js /app/admin-proxy.js
CMD ["node", "/app/gateway.js"]
