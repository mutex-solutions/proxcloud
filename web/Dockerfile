# Multi-app FE image builder: --build-arg APP=client|admin
FROM node:20-alpine AS build
ARG APP
WORKDIR /app
COPY ${APP}/package.json ${APP}/package-lock.json ./
RUN npm ci
COPY ${APP}/ ./
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
COPY docker-entrypoint.sh /docker-entrypoint-proxcloud.sh
# Vite preserves copied public/ file modes, including a checkout's restrictive umask.
# Normalize only nginx's public assets, not application sources or secrets.
RUN chmod 0755 /docker-entrypoint-proxcloud.sh \
    && find /usr/share/nginx/html -type d -exec chmod 0755 {} + \
    && find /usr/share/nginx/html -type f -exec chmod 0644 {} +
EXPOSE 80
ENTRYPOINT ["/docker-entrypoint-proxcloud.sh"]
