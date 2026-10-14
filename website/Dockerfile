FROM node:22-alpine AS docs
WORKDIR /src
COPY docs-site/package.json docs-site/package-lock.json ./
RUN npm ci
COPY docs-site/ .
ENV ASTRO_BASE=/docs/
ENV ASTRO_SITE=https://agentic.hosting
RUN npm run build

FROM nginx:alpine
COPY website/nginx.conf /etc/nginx/nginx.conf
COPY website/index.html /usr/share/nginx/html/index.html
COPY website/install.sh /usr/share/nginx/html/install.sh
COPY website/install-cli.sh /usr/share/nginx/html/install-cli.sh
COPY website/install-skill.sh /usr/share/nginx/html/install-skill.sh
COPY website/changelog /usr/share/nginx/html/changelog
COPY llms.txt /usr/share/nginx/html/llms.txt
COPY llms-full.txt /usr/share/nginx/html/llms-full.txt
COPY --from=docs /src/dist /usr/share/nginx/html/docs
