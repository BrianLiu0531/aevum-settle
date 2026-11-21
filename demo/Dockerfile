FROM node:22-alpine
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY . .
# Invoices live in SQLite under /app/data (mount a volume there to keep them across deploys)
RUN mkdir -p /app/data && chown node:node /app/data
USER node
ENV MERCHANT_DB=/app/data/merchant.db
EXPOSE 8787
CMD ["node", "merchant.js"]
