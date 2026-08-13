---
name: use-docker-buildx-for-building
description: Use when building or validating Docker images for repository components with Docker Buildx, especially when the Dockerfile, build context, target platforms, image namespace, component name, or runtime smoke check must be determined from the current repository.
---

# Use Docker Buildx for Building

## Core Principle

Derive every build command from repository inspection. Inspect the current user, Git remote and organization or namespace, repository name, component name, Dockerfile, build context, target platform, and runtime contract yourself. Do not provide or rely on a naming helper or build helper script.

## Workflow

1. Read every applicable `AGENTS.md` and the repository's build documentation before forming a command.
2. Inspect `id -un`, the Git root, configured Git remotes, the current branch's tracking remote, and all relevant Dockerfiles.
3. Read the selected Dockerfile and related manifests. Derive the positional build context only from local `COPY` and `ADD` sources in the default local context, and ensure those sources resolve inside it. Classify `COPY --from` sources as stages, images, or named contexts; identify named contexts and remote `ADD` sources without treating them as default-local sources. Determine any required `--build-context` inputs and authentication separately.
4. Determine the component name and target platform from repository evidence, deployment manifests, CI configuration, and the intended runtime. Ask the user when the component or platform remains ambiguous.
5. Select the current branch's tracking remote first, then a remote designated by repository build documentation. Ask the user when neither source identifies one remote or when candidates remain. Form the image name literally as `test.<current-username>.local/<org-or-namespace>/<repo-name>/<component-name>:<tag>`. Substitute every segment with an observed value, derive the organization or namespace from the selected remote, and use `latest` only when no tag was requested or established by repository convention. Validate the namespace, repository, component, tag, and complete name as a legal image reference. If normalization would change identity and no repository convention resolves it, ask instead of guessing.
6. Make each local smoke build, loaded tag, and verification correspond to exactly one explicit platform. Build, load, tag, and verify multiple target platforms separately. Construct every build command with an explicit `--platform <platform>`, `-f <dockerfile>`, the inspected build context, `--tag <image-name>`, and `--load`. Repeat the explicit `--platform` option in every build command; never rely on an ambient platform default. Before running a non-host platform, confirm support in the builder, local image store, emulation layer, and container runtime.
7. Always retain `--load`; the subsequent local `docker run` verification depends on the image being loaded into the local Docker image store.
8. Inspect the built image, then actually run and verify the same target platform according to its `ENTRYPOINT`, `CMD`, exposed ports, required arguments, and documented runtime contract. Pass `--platform <platform>` explicitly to `docker run` when the target is not the host platform or platform selection could be ambiguous; omit it only for a host-platform image loaded under a single-platform tag. For a one-shot command, use `docker run --rm`. For a long-running service, assign a unique container name or capture its container ID, start it in the background, bind published ports only to loopback, and prefer a dynamic host port when possible. Perform a bounded readiness, health, or protocol check, and use a trap or `finally` cleanup to stop and `rm -f` that exact container even when startup or verification fails.
9. Keep secrets out of `--build-arg`, Dockerfile `ARG` and `ENV`, command-line literals, `COPY`, and the build context. Inspect `.dockerignore` to ensure credentials are excluded. For private dependencies, use BuildKit `--secret` or `--ssh` with matching `RUN --mount=type=secret` or `RUN --mount=type=ssh` consumption so secrets do not enter image layers.

## Quick Reference

| Decision | Evidence to inspect |
| --- | --- |
| Current username | `id -un` |
| Repository root and name | Git root and its basename |
| Selected remote and namespace | Current branch tracking remote, then repository build documentation |
| Component name | Dockerfile purpose and path, manifests, build docs, CI configuration |
| Dockerfile | Repository Dockerfile inventory and component documentation |
| Build context | Default-local `COPY` and `ADD` sources, source classification, repository layout |
| Target platform | Deployment manifests, CI configuration, base-image support, runtime target |
| Runtime smoke check | Dockerfile `ENTRYPOINT`, `CMD`, `EXPOSE`, health checks, and runtime docs |
| Image reference validity | Docker image-reference grammar and repository naming conventions |
| Secret handling | `.dockerignore`, private-dependency requirements, BuildKit secret or SSH mounts |

## Fictional Example

Treat the following values as fictional, not defaults. Reinspect them on every use: user `mia`, remote `acme/widgets`, component `api`, Dockerfile `services/api/Dockerfile`, repository-root context, and platform `linux/arm64`.

```sh
docker buildx build --platform linux/arm64 -f services/api/Dockerfile . --tag test.mia.local/acme/widgets/api:latest --load
docker run --rm test.mia.local/acme/widgets/api:latest --version
```

## Common Mistakes

- Do not guess the build context or mistake `COPY --from`, named contexts, or remote `ADD` sources for default-local sources.
- Do not hardcode usernames, namespaces, repository names, components, tags, Dockerfiles, contexts, or platforms.
- Do not omit `--load` from a build intended for local verification.
- Do not stop after a successful build; inspect and actually run the loaded image.
- Do not leave a service running, use an unbounded readiness check, expose smoke-test ports beyond loopback, or clean up a container other than the exact one started.
- Do not pass secrets through build arguments, Dockerfile variables, literals, copied files, or the build context; use BuildKit secret or SSH mounts and exclude credentials with `.dockerignore`.
