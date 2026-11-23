FROM node:24-alpine
WORKDIR /app
COPY gateway/gateway-alarm-clock.js /app/gateway.js
CMD ["node", "/app/gateway.js"]
