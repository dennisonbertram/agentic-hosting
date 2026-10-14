FROM node:24.15.0-alpine3.22@sha256:b689d4005875ae167178471a7a622ec2909459a3bbb32277260be1971af7a99f

ENV NODE_ENV=production PORT=3000
WORKDIR /app

COPY --chown=node:node package.json ./
COPY --chown=node:node app.mjs server.mjs ./

USER node
EXPOSE 3000
CMD ["node", "server.mjs"]
