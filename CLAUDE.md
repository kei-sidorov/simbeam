# CLAUDE.md — инструкции для агента

Ты помогаешь разрабатывать **simcast** — стриминг iOS-симулятора с Mac на iPad для удалённой
разработки. Прежде чем писать код, прочитай:

1. `README.md` — что строим и какой стек.
2. `docs/ARCHITECTURE.md` — почему именно так (контекст всех решений).
3. `docs/ROADMAP.md` — фазы и Definition of Done каждой.
4. `docs/decisions.md` — лог принятых решений.

## Правила работы

- **Не переписывай движок.** Видео и инжект событий делает `idb_companion` (внешний бинарь).
  Мы пишем тонкую обвязку над его gRPC/CLI, а не свой CoreSimulator-пайплайн.
- **Скоуп — только симуляторы.** Не добавляй код под реальные устройства.
- **Вертикальные срезы.** Делай фазу целиком до запускаемого результата, не строй слои впрок.
- **Не делай отложенное.** То, что в «Сознательно отложено» (`ARCHITECTURE.md`) — не трогай
  без явной просьбы (адаптивный битрейт, ScreenCaptureKit, TURN, бандлинг companion, нотаризация).
- Каждое новое архитектурное решение фиксируй строкой в `docs/decisions.md`.
- Делай атомарные коммиты по ходу. Ветку от текущей, если просят интеграцию.

## Окружение (проверено у автора)

- `idb_companion` стоит: `/opt/homebrew/bin/idb_companion`, версия 1.1.8.
- `idb.proto` доступен: `~/Developer/ios-bridge/.venv/proto/idb.proto` (можно скопировать в репо).
- Xcode/Command Line Tools установлены (без них симуляторов нет).
- Go — проверь версию через `go version`; если не стоит, скажи пользователю поставить.

## Текущая задача: Phase 0 — Bootstrap

Полное описание и DoD — в `docs/ROADMAP.md` → Phase 0. Кратко:

1. `go mod init` (спроси у пользователя module path, или предложи дефолт
   `github.com/<github-user>/simcast` и подтверди).
2. Проверка окружения: найти `idb_companion`, напечатать его версию.
3. `internal/companion`: запустить `idb_companion --list 1`, распарсить JSON в `[]Simulator`
   (UDID, name, OS version, state).
4. `cmd/simcastd` с подкомандой `list` → печатает реальные симуляторы.
5. Обнови README инструкцией запуска.

**Definition of Done:** `go run ./cmd/simcastd list` печатает РЕАЛЬНЫЙ список симуляторов
с машины (не заглушку). Сначала глянь формат вывода: запусти сам `idb_companion --list 1` и
`idb_companion --help`, разберись в JSON, потом парси.

После Bootstrap покажи результат и предложи перейти к Phase 1 (JPEG MVP).

---

## Стартовый промпт (для пользователя)

Скопируй это первым сообщением агенту в папке `~/Developer/simcast`:

> Прочитай README.md, CLAUDE.md и docs/ (ARCHITECTURE, ROADMAP, decisions). Затем выполни
> **Phase 0 (Bootstrap)** из ROADMAP. Перед парсингом сам запусти `idb_companion --version`,
> `idb_companion --help` и `idb_companion --list 1`, разберись в реальном формате вывода.
> Спроси у меня module path для go.mod (или предложи дефолт). Доведи до Definition of Done:
> `go run ./cmd/simcastd list` печатает настоящий список моих симуляторов. По ходу делай
> атомарные коммиты и зафиксируй новые решения в docs/decisions.md.
