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
RUN chmod +x /docker-entrypoint-proxcloud.sh
EXPOSE 80
ENTRYPOINT ["/docker-entrypoint-proxcloud.sh"]
