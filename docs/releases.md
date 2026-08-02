# Release Workflow

Этот документ — источник истины для релизов `sonarr-torrent-importer`: он определяет итоговый артефакт, правила версионирования, публикацию Docker-образа, требования к контейнеру и пользовательское обновление. Обычные команды разработки описаны отдельно, а исходные продуктовые гипотезы и историческое ревью остаются в [`project-context.md`](project-context.md) и [`concept-review.md`](concept-review.md).

В репозитории есть production workflow с durable qBittorrent rename recovery, Sonarr import verification, rolling revision reconciliation, multi-stage Dockerfile, CI и tag-triggered release workflow. Текущие команды сборки и проверки определены в [`development.md`](development.md); этот документ остаётся контрактом публикации образа.

## Релизный контракт

- Релизы выпускаются только из ветки `main`.
- Версии следуют Semantic Versioning и используют Git-теги `vX.Y.Z`, например `v1.0.0`.
- Главный и единственный deployable-артефакт релиза — готовый OCI/Docker-образ. Для запуска пользователю не нужны checkout репозитория, компилятор или package manager.
- Образ публикуется в GitHub Container Registry:

  ```text
  ghcr.io/zenderg/sonarr-torrent-importer
  ```

- Push release-тега запускает GitHub Actions workflow.
- Workflow сначала выполняет все проверки, затем собирает и публикует образ. Неуспешная проверка не должна оставлять опубликованный релизный образ.
- Один релиз собирается для `linux/amd64` и `linux/arm64` и публикуется как один multi-platform image manifest. Docker автоматически выбирает подходящую архитектуру.
- Workflow создаёт draft GitHub Release с digest образа, release notes и готовым Docker Compose примером.
- Финальные пользовательские release notes проверяются перед ручной публикацией GitHub Release.
- Release notes хранятся в GitHub Releases. Отдельный хронологический `CHANGELOG.md` не требуется.

## Теги образа

Стабильный релиз `v1.1.0` публикует один и тот же образ под тегами:

```text
ghcr.io/zenderg/sonarr-torrent-importer:v1.1.0
ghcr.io/zenderg/sonarr-torrent-importer:1.1.0
ghcr.io/zenderg/sonarr-torrent-importer:latest
```

Все три ссылки должны разрешаться в один digest. Версионированный тег — рекомендуемый вариант для Docker Compose; `latest` предназначен только для ручного ознакомления и не гарантирует воспроизводимое обновление.

Предварительные версии используют теги вида `v1.1.0-rc.1`. Они не обновляют `latest`.

При необходимости точной фиксации пользователь может указать digest:

```text
ghcr.io/zenderg/sonarr-torrent-importer@sha256:<digest>
```

## Содержимое образа

Production-образ должен содержать только то, что необходимо приложению во время исполнения:

- собранное приложение и production-зависимости;
- Sonarr и qBittorrent API adapters;
- mapper и orchestration logic;
- durable JSON safety journal и межпроцессная блокировка execute;
- health/status endpoint;
- необходимые CA certificates и минимальный runtime.

В образ не входят:

- Sonarr, qBittorrent или indexer;
- пользовательские API-ключи, пароли и tracker credentials;
- пользовательский `/data` safety journal;
- torrent data и медиатека;
- исходники, test fixtures, компиляторы и development-зависимости, если они не нужны runtime.

Sonarr и qBittorrent остаются самостоятельными внешними сервисами. `sonarr-torrent-importer` подключается к ним по API и не пытается объединять весь media stack в один контейнер.

## Runtime-контракт контейнера

Реализация должна сохранять следующие стабильные границы:

- приложение запускается непривилегированным пользователем;
- постоянное состояние хранится под `/data`;
- HTTP status API слушает `0.0.0.0:8080` по умолчанию;
- `GET /health` используется для container healthcheck;
- процесс корректно обрабатывает `SIGTERM` и завершает текущую операцию безопасным образом;
- при старте валидируется конфигурация, а ошибки сообщаются без вывода секретов;
- schema version durable operation journal проверяется до восстановления операции;
- контейнер не требует Docker socket и привилегированного режима.

Обычному queue-based importer каталог downloads не требуется: manifest и rename postconditions читаются через qBittorrent API. Rolling releases включаются отдельным Compose overlay, который монтирует то же qBittorrent storage в importer read/write и добавляет его storage group. Доступ нужен только для revision-isolated copy/recheck; старый data tree не изменяется, hardlink не создаётся, автоматической очистки нет.

Минимальная runtime-конфигурация:

```text
HOST=0.0.0.0
PORT=8080
DATA_ROOT=/data
SONARR_URL=<Sonarr API base URL>
SONARR_API_KEY=<secret>
QBITTORRENT_URL=<qBittorrent WebUI base URL>
QBITTORRENT_USERNAME=<secret>
QBITTORRENT_PASSWORD=<secret>
REQUEST_TIMEOUT=30s
COMMAND_TIMEOUT=10m
WORKFLOW_TIMEOUT=30m
POLL_INTERVAL=2s
```

Опциональная rolling-конфигурация добавляет `PROWLARR_URL`, `PROWLARR_API_KEY`, `QBITTORRENT_MEDIA_ROOT`, `SONARR_MEDIA_ROOT`, `IMPORTER_MEDIA_ROOT`, `REVISION_POLL_INTERVAL`, а Compose overlay — `QBITTORRENT_MEDIA_HOST_PATH` и `QBITTORRENT_MEDIA_GID`. `SONARR_MEDIA_ROOT` задаёт тот же storage в namespace Sonarr и сохраняется в каждой revision как переведённый durable path. Эти изменения совместимы: без полного rolling-блока сервис запускает прежний queue-based workflow.

Категории остаются настройкой Sonarr download client. Importer наблюдает ожидаемое post-import изменение категории, но не встраивает deployment-specific category names в образ.

## Требования к Docker-сборке

Dockerfile должен быть multi-stage:

1. Dependency stage устанавливает зависимости из lockfile, если выбранный ecosystem создаёт lockfile; текущий stdlib-only Go module внешних модулей не имеет.
2. Build/validation stage выполняет обязательные статические проверки, полезные тесты и production build.
3. Runtime stage получает только готовое приложение и production-зависимости.

Release workflow должен проверять не только сборку исходников, но и поставляемый контейнер:

- установка зависимостей воспроизводима по lockfile;
- версия тега совпадает с версией в каноническом manifest проекта и lockfile, если выбранный ecosystem хранит там версию;
- production image успешно собирается;
- контейнер запускается без root privileges;
- приложение отвечает на `/health`;
- некорректная обязательная конфигурация приводит к понятной ошибке и ненулевому exit code;
- в metadata образа записаны версия, commit SHA, repository URL и build time через стандартные `org.opencontainers.image.*` labels;
- итоговый runtime image не содержит секретов или файлов локального окружения.

Набор lint, typecheck и test команд определяется после выбора стека. Проверки должны выполняться в том же build stage и runtime-контексте, из которых производится релиз, чтобы локальная среда GitHub runner не скрывала отсутствующие зависимости.

## Подготовка релиза

1. Убедиться, что `main` синхронизирован с `origin/main` и все обязательные проверки успешны.
2. Выбрать следующую версию по Semantic Versioning:
   - patch — совместимое исправление;
   - minor — совместимая пользовательская возможность;
   - major — несовместимое изменение конфигурации, данных или API.
3. Если выбранный ecosystem хранит версию в manifest или lockfile, обновить её. Текущий Go binary получает версию из release-тега во время Docker build и отдельного version-файла не имеет.
4. Если подготовка релиза изменила файлы, закоммитить её conventional commit сообщением:

   ```bash
   git add <version-files>
   git commit -m "chore: prepare vX.Y.Z"
   ```

5. Просмотреть изменения с предыдущего релиза:

   ```bash
   git describe --tags --abbrev=0
   git log --oneline <previous-tag>..HEAD
   ```

   Для первого релиза используется `git log --oneline HEAD`.

6. Создать и отправить аннотированный тег:

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

7. Дождаться завершения workflow `Release`.
8. Проверить draft GitHub Release, digest и Compose пример.
9. Отредактировать пользовательские release notes и опубликовать GitHub Release.

Если workflow пришлось перезапустить, он не должен перезаписывать вручную отредактированные release notes существующего GitHub Release.

## Содержимое release notes

Release notes должны кратко отвечать на вопросы пользователя:

- что добавлено;
- что исправлено;
- есть ли breaking changes;
- изменились ли environment variables, volumes, права доступа или network requirements;
- содержит ли релиз миграцию данных и совместим ли откат;
- какой versioned image следует использовать;
- каков опубликованный digest.

Conventional commit prefixes `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:` и `build:` помогают составить notes, но итоговый текст пишется по фактическому diff, а не генерируется слепо из заголовков коммитов.

## Пользовательский deployment

Базовый Compose-файл стабильного релиза будет выглядеть так:

```yaml
services:
  importer:
    image: ghcr.io/zenderg/sonarr-torrent-importer:vX.Y.Z
    restart: unless-stopped
    stop_grace_period: 35m
    ports:
      - "127.0.0.1:${IMPORTER_HOST_PORT:-8080}:8080"
    env_file:
      - .env
    volumes:
      - importer-data:/data
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    read_only: true

volumes:
  importer-data:
```

Секреты хранятся в локальном `.env`, который не коммитится:

```dotenv
IMPORTER_HOST_PORT=8080
SONARR_URL=http://sonarr:8989
SONARR_API_KEY=replace-me
QBITTORRENT_URL=http://qbittorrent:8080
QBITTORRENT_USERNAME=replace-me
QBITTORRENT_PASSWORD=replace-me
```

Адрес `localhost` внутри контейнера указывает на сам контейнер. Если Sonarr и qBittorrent работают в других контейнерах, они должны быть доступны через общую Docker network; если они работают на другом хосте, используются доступные контейнеру LAN/VPN адреса.

Запуск и проверка:

```bash
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
docker compose -f compose.example.yaml ps
docker compose -f compose.example.yaml logs --tail=100 importer
```

После первого publish нужно отдельно проверить, что GHCR package публичный и связан с GitHub repository. Иначе анонимный `docker compose -f compose.example.yaml pull` потребует авторизацию.

## Обновление и откат

Для обновления пользователь меняет только versioned tag и перезапускает Compose:

```bash
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
```

Перед релизом с изменением durable operation schema release notes должны явно описывать совместимость отката. Автоматический backup не считается гарантированным, пока он отдельно не реализован и не проверен; перед таким обновлением пользователь должен остановить контейнер и сохранить копию каталога `/data`.

`v1.1.0` сохраняет совместимость с version 2 JSON records обычного importer и добавляет отдельные version 1 rolling records и immutable torrent artifacts под `/data/rolling`. Необратимой миграции существующих records нет. `v1.0.0` игнорирует новый rolling-каталог, поэтому завершённая новая revision продолжит сидироваться, но больше не будет обновляться. Откат во время активной rolling operation запрещён: сначала её нужно завершить на `v1.1.0` либо сохранить `/data` и вернуть `v1.1.0` для reconciliation. Перед обновлением или откатом сохраните копию `/data`.

## Готовность релиза

Первый релиз считается готовым, когда выполнены все условия:

- в репозитории есть production Dockerfile и `.dockerignore`;
- есть пользовательский Compose пример и безопасный `.env.example`;
- GitHub Actions собирает и проверяет image по pull request или commit без публикации;
- push `vX.Y.Z` публикует multi-platform image в GHCR;
- все version tags релиза указывают на один digest;
- draft GitHub Release создаётся автоматически;
- новый deployment запускается на чистом хосте только из Compose и опубликованного образа;
- write-ahead operation state переживает пересоздание контейнера благодаря `/data` volume, а повтор execute не дублирует доказанный qBittorrent rename или неопределённый ManualImport;
- реальный Compose E2E принудительно перезапустил importer в `rename_file_submitting` и подтвердил один rename-запрос, `[01].mkv` → canonical path, Sonarr auto-import, import history/episode-file, post-import category, сохранение active seeding и queue finalization;
- rolling Compose E2E подтвердил verified reuse, `copy`-импорт только новой серии, post-import full recheck, сохранность старых байтов и ровно по одному add/delete запросу через два forced restart;
- GitHub Release прикладывает `compose.example.yaml`, `compose.rolling.example.yaml` и `.env.example`;
- Sonarr и qBittorrent credentials отсутствуют в image layers, logs и repository;
- документированный upgrade и допустимый rollback проверены вручную хотя бы один раз.
