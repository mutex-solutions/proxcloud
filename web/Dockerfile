# Multi-app FE image builder: --build-arg APP=client|admin
FROM node:24.22.2-alpine AS build
ARG APP
WORKDIR /app
COPY ${APP}/package.json ${APP}/package-lock.json ./
RUN npm ci
COPY ${APP}/ ./
RUN npm run build

FROM nginx:1.30.7-alpine
# Refresh the distro-patched XSLT and XML libraries (libxslt: CVE-2026-87636/
# CVE-2026-9947; libxml2: CVE-2026-86144); a fresh base pull alone may still
# carry the earlier Alpine package revision.
RUN apk upgrade --no-cache libxslt libxml2
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
