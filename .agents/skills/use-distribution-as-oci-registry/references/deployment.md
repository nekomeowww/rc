# Reproducible Docker Compose Deployment

Create the following bundle in the user-selected directory. Replace only parameter values; do not copy values from another deployment.

```text
registry/
├── compose.yaml
├── .env
├── auth/htpasswd
├── certs/fullchain.pem
├── certs/private.key
├── nginx/default.conf.template
└── data/
```

## Compose file

```yaml
name: distribution-registry

services:
  distribution:
    image: registry:3.1.1
    restart: unless-stopped
    environment:
      REGISTRY_HTTP_ADDR: 0.0.0.0:5000
      REGISTRY_HTTP_SECRET: ${REGISTRY_HTTP_SECRET:?REGISTRY_HTTP_SECRET is required}
      REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY: /var/lib/registry
      REGISTRY_AUTH: htpasswd
      REGISTRY_AUTH_HTPASSWD_REALM: Registry
      REGISTRY_AUTH_HTPASSWD_PATH: /auth/htpasswd
    volumes:
      - ./data:/var/lib/registry
      - ./auth:/auth:ro

  nginx:
    image: nginx:1.28-alpine
    restart: unless-stopped
    depends_on:
      - distribution
    environment:
      REGISTRY_DOMAIN: ${REGISTRY_DOMAIN:?REGISTRY_DOMAIN is required}
    ports:
      - 80:80
      - 443:443
    volumes:
      - ./nginx/default.conf.template:/etc/nginx/templates/default.conf.template:ro
      - ./certs:/etc/nginx/certs:ro
```

## Nginx template

```nginx
server {
    listen 80;
    server_name ${REGISTRY_DOMAIN};
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name ${REGISTRY_DOMAIN};

    ssl_certificate     /etc/nginx/certs/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/private.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    client_max_body_size 0;
    chunked_transfer_encoding on;

    location /v2/ {
        proxy_pass http://distribution:5000;
        proxy_set_header Host              $http_host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_redirect off;
        proxy_request_buffering off;
        proxy_read_timeout 900;
        proxy_send_timeout 900;
    }
}
```

The proxy terminates HTTPS and connects to Distribution over the private Compose network with HTTP. `X-Forwarded-Proto` prevents Distribution from returning downgraded HTTP upload locations.

The redirect assumes the public service uses standard ports 80 and 443. When testing with arbitrary host ports, validate HTTPS directly or generate an explicit redirect authority; `$host` intentionally omits the incoming port.

## Initialize

Create directories with restrictive permissions. Generate the HTTP secret into `.env`, set `REGISTRY_DOMAIN`, and exclude `.env`, `auth/`, `certs/`, and `data/` from version control. Obtain a trusted certificate whose names include the registry hostname and place the complete chain and private key at the paths above.

Generate the password file interactively so the password is not placed in shell history:

```sh
docker run --rm -it \
  -v "$PWD/auth:/auth" \
  --entrypoint htpasswd \
  httpd:2.4-alpine \
  -Bc /auth/htpasswd REGISTRY_USERNAME
```

Validate and start:

```sh
docker compose config
docker compose up -d
docker compose ps
docker compose logs --tail=100 distribution nginx
```

## End-to-end verification

The unauthenticated endpoint must return `401` plus the Registry API and authentication headers:

```sh
curl -sS -D - -o /dev/null "https://${REGISTRY_DOMAIN}/v2/"
```

Do not add `-k` to the acceptance check. A self-signed loopback test may work because some Docker daemons treat `127.0.0.0/8` as insecure; that does not validate the certificate chain clients will use in production.

Then perform a real write/read test:

```sh
docker login "$REGISTRY_DOMAIN"
docker pull busybox:latest
docker tag busybox:latest "$REGISTRY_DOMAIN/smoke/busybox:latest"
docker push "$REGISTRY_DOMAIN/smoke/busybox:latest"
docker image rm "$REGISTRY_DOMAIN/smoke/busybox:latest"
docker pull "$REGISTRY_DOMAIN/smoke/busybox:latest"
```

For a multi-platform image, verify the remote OCI index:

```sh
docker buildx imagetools inspect "$REGISTRY_DOMAIN/project/component:tag"
```

Do not report success until the push and subsequent pull both complete.

Sources:

- https://distribution.github.io/distribution/about/deploying/
- https://distribution.github.io/distribution/about/configuration/
- https://distribution.github.io/distribution/recipes/nginx/
