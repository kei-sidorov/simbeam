# Phase 1 — JPEG MVP — Design

**Дата:** 2026-06-02
**Статус:** утверждён, готов к планированию
**Контекст:** `docs/ROADMAP.md` → Phase 1; `docs/ARCHITECTURE.md`; продолжение Phase 0 (bootstrap).

## Цель

Самый дешёвый сквозной стрим без WebRTC: на debug-клиенте в браузере видно живой экран
симулятора, клик проходит как тап. Проверяем gRPC-связку с `idb_companion` (describe /
video_stream / hid) до того, как браться за WebRTC в Phase 2.

**Definition of Done (из ROADMAP):** на «клиенте» видно живой экран симулятора, клик проходит
как тап в симулятор. Дополнительно (уточнено при дизайне): клиент показывает список симуляторов,
бутит выбранный кнопкой, и только после этого начинает стриминг.

## Скоуп

**В Phase 1:**
- gRPC-стабы из `idb.proto` (`grpc-go`), подключение к сайдкару `idb_companion --udid X --grpc-port N`.
- `describe` → размеры экрана для маппинга координат.
- `video_stream(format=MJPEG)` → поток JPEG-кадров → WS как binary.
- `hid` → `tap(x,y)` (touch down+up) и кнопка `Home`.
- REST `list` / `boot`. Debug-клиент (отдельный файл) со списком, кнопкой boot и кликабельным кадром.

**НЕ в Phase 1 (Phase 2+ / «Сознательно отложено»):**
- WebRTC, pion, signaling, DataChannel.
- Swipe, клавиатура, прочие кнопки (`hid` только tap + Home).
- Адаптивный битрейт, свой видеопайплайн.
- Бандлинг companion'а, дистрибуция.

## Архитектура

```
Browser (debug-клиент, web/debug/index.html — отдельный файл)
  │  GET  /api/simulators ─────────► simbeamd ──► companion.List()        (CLI --list 1)
  │  POST /api/boot {udid} ────────► simbeamd ──► companion.Boot(udid)    (CLI --boot)
  │  WS   /session?udid=X ─────────► simbeamd
  │        ▼ binary: JPEG-кадры              │ spawn: idb_companion --udid X --grpc-port N
  │        ▲ JSON: {type:"tap",x,y}/{home}   │ gRPC: describe · video_stream(MJPEG) · hid
  ▼                                           ▼
                                        iOS Simulator (CoreSimulator)
```

Один WS = одна изолированная сессия со своим сайдкаром и своим gRPC-портом. list/boot —
глобальные REST. Стриминг кадров и ввод — на WS.

### Поток жизни сессии

1. Браузер открывает страницу → `GET /api/simulators` → список симуляторов.
2. «Boot» у выбранного → `POST /api/boot {udid}` → `idb_companion --boot <udid>` (блокирующе,
   `--verify-booted` ждёт готовности). Уже booted → возврат сразу (идемпотентно).
3. Сим `Booted` → клиент открывает `WS /session?udid=X`.
4. На WS-коннект simbeamd:
   - спавнит сайдкар `idb_companion --udid X --grpc-port N` (свободный порт),
   - ждёт готовности gRPC (dial + успешный `describe`),
   - шлёт клиенту `{"type":"ready","w":W,"h":H}`,
   - открывает `video_stream(MJPEG)` → каждый кадр в WS как binary,
   - открывает `hid`-стрим → принимает JSON-тапы из WS.
5. WS закрылся (или ошибка стрима) → отмена контекста → закрытие стримов → **сайдкар убит**.
   Симулятор остаётся booted (shutdown — только явной командой, которой в Phase 1 нет).

## Раскладка пакетов

```
simbeam/
├── proto/idb.proto              # копия из ~/Developer/ios-bridge/.venv/proto/idb.proto
├── internal/
│   ├── idbpb/                   # СГЕНЕРЁННЫЕ стабы (коммитятся в git, руками не правим)
│   ├── companion/               # CLI-обвязка (Phase 0) + Boot()
│   │   └── companion.go         # Resolve / Version / List / Boot(ctx, udid)
│   ├── idb/                     # gRPC-обвязка над idbpb
│   │   ├── sidecar.go           # Spawn(ctx, udid, port) → готовность; Close() убивает процесс
│   │   └── client.go            # Describe / VideoStream / Tap / Home
│   └── server/
│       ├── server.go            # роутер: /api/simulators, /api/boot, /session, (опц.) static
│       └── session.go           # WS-сессия: сайдкар + кадры↓ + ввод↑ + teardown
├── web/debug/index.html         # debug-клиент (раздаётся только при --web)
├── Makefile                     # proto / build / run / test
├── scripts/gen-proto.sh         # вызывается из Makefile (опционально)
└── cmd/simbeamd/main.go         # list (есть) + serve [--addr :8080] [--web ./web/debug]
```

**`companion` vs `idb`:** `companion` — CLI-флаги companion'а (lifecycle: list/boot/version),
без gRPC. `idb` — gRPC к уже запущенному сайдкару (describe/video_stream/hid), обёртка над
`idbpb`. Держим две природы companion'а (CLI-сервер vs gRPC-API) в разных пакетах.

## Генерация стабов

- Копируем `idb.proto` в `proto/`.
- `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` и
  `google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` (в `$HOME/go/bin`).
- `make proto` → `protoc --go_out=internal/idbpb --go-grpc_out=internal/idbpb proto/idb.proto`
  (с корректными `paths=source_relative` / `go_package`).
- Сгенерённый код **коммитим** (сборка у потребителя не требует protoc).
- Новые зависимости: `google.golang.org/grpc`, `google.golang.org/protobuf`.
- protoc 33.4 уже стоит на машине автора; плагины ставятся `go install`.

## Контракты API

### REST

- `GET /api/simulators` → `200` `[{ "udid","name","os_version","state" }, ...]`
  (отфильтрованы только Simulator-таргеты, как в Phase 0).
- `POST /api/boot` body `{"udid":"..."}`:
  - `200 {"state":"Booted"}` при успехе (в т.ч. уже booted);
  - `4xx/5xx {"error":"..."}` при неизвестном udid / таймауте / ошибке.
  - Блокирующий, с таймаутом (по умолчанию ~120s).

### WS `/session?udid=X`

**Сервер → клиент:**
- binary-сообщение = сырой JPEG-кадр (как из MJPEG).
- text/JSON: `{"type":"ready","w":W,"h":H}` (один раз после готовности),
  `{"type":"error","msg":"..."}` (перед закрытием при ошибке).

**Клиент → сервер (JSON):**
- `{"type":"tap","x":<0..1>,"y":<0..1>}` — нормализованные доли показанного кадра.
- `{"type":"home"}`.

### Маппинг координат

- Клиент шлёт нормализованные доли `[0,1]` (resolution-independent; не зависит от
  `scale_factor` MJPEG-кадра).
- Сервер берёт размеры из `describe` (`ScreenDimensions`) и умножает: `point = {x*W, y*H}`.
- **Research-item:** `hid` `HIDTouch.Point` ждёт пиксели (`width`/`height`) или логические
  точки (`width_points`/`height_points`)? Дефолт — пиксели; проверяем живым тапом (клик по
  углу кнопки → попадание) в рамках DoD. Если не попадает — переключаем на `*_points`.

### hid-последовательности

- `tap` = `HIDPress{action: touch(point), DOWN}` затем `HIDPress{..., UP}` (два сообщения в стрим).
- `home` = `HIDPress{action: button HOME, DOWN}` + `{..., UP}`.
- `hid` — client-streaming RPC: открываем один стрим на сессию, шлём события по мере прихода.

## Ключевые решения реализации

- **Выбор порта сайдкара:** `net.Listen("tcp", "127.0.0.1:0")`, забираем порт, закрываем,
  передаём в `--grpc-port`. Гонка close→bind маловероятна и приемлема для MVP.
- **Готовность сайдкара:** после спавна — ретраи dial + `describe` с бэк-оффом и общим
  таймаутом (~15s). Успех = готов; иначе teardown + ошибка в WS.
- **Backpressure кадров:** single-slot «последний кадр». Reader из gRPC кладёт кадр в слот
  (затирая старый); writer в WS отправляет последний доступный. Кадры дропаются, не копятся —
  latency не накапливается.
- **`serve`:** API раздаётся всегда; static debug-клиент — только при `--web <dir>` (бинарь
  про API, не про конкретный UI; реальные клиенты подключатся к REST+WS позже).

## Обработка ошибок

- `idb_companion` не найден → `serve` падает на старте с понятной ошибкой (как в Phase 0).
- Неизвестный/не booted udid в `/session` → `{"type":"error"}` + закрытие WS (клиент должен
  сперва забутить).
- Спавн сайдкара/готовность не достигнута → teardown процесса + ошибка в WS.
- `video_stream` оборвался или `hid` отдал ошибку → закрываем сессию, убиваем сайдкар.
- WS закрыт клиентом → отмена контекста сессии, закрытие gRPC-стримов, kill сайдкара.
- Каждая сессия владеет своим сайдкаром; обязателен teardown во всех путях выхода (`defer`).

## Тестирование

**Юнит (без симулятора):**
- Маппинг нормализованных координат → пиксели устройства (чистая функция).
- Парсинг control-сообщений WS (валидный/битый JSON, неизвестный type).
- Логика single-slot буфера кадров (затирание, отсутствие накопления).
- Хелпер выбора свободного порта.
- `companion.Boot` — разбор успех/ошибка (как List в Phase 0, с фейковым бинарём при возможности).

**Интеграция / ручная проверка DoD (нужен реальный сим):**
- `make run` (или `go run ./cmd/simbeamd serve --web ./web/debug`) → открыть браузер →
  увидеть список → забутить сим → увидеть живые кадры → клик попадает как тап → Home работает.
- Проверка попадания координат (клик по углу элемента → реакция в нужном месте).

## Команды (Makefile)

- `make proto` — генерация стабов в `internal/idbpb`.
- `make build` — `go build ./...`.
- `make run` — `go run ./cmd/simbeamd serve --web ./web/debug`.
- `make test` — `go test ./...`.

## Открытые вопросы (в impl)

1. `hid` координаты: пиксели vs логические точки (см. маппинг). Решается живым тапом.
2. Параметры MJPEG (`fps`, `compression_quality`, `scale_factor`) — подобрать стартовые
   значения по ощущению (напр. fps=30, scale=1.0); не блокирует DoD.
