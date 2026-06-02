# Roadmap (по фазам)

Принцип: каждая фаза — вертикальный срез, который можно запустить и **увидеть результат**.
Не строим горизонтальные слои «впрок». Простое → сложное. Откладываем всё из раздела
«Сознательно отложено» в `ARCHITECTURE.md`.

---

## Phase 0 — Bootstrap ⬅️ начинаем здесь

**Цель:** доказать сквозную связку «наш Go-код → `idb_companion` → реальные симуляторы».
Это «hello world», который проверяет всю фундаментальную цепочку.

**Deliverables:**
1. `go.mod` инициализирован.
2. Проверка окружения: `idb_companion` найден в PATH, печатаем его версию (`idb_companion --version`).
3. Пакет `internal/companion`:
   - функция, которая запускает `idb_companion --list 1`, парсит JSON-вывод в `[]Simulator`
     (UDID, name, OS version, state).
4. `cmd/simcastd` с подкомандой `list`, которая печатает доступные симуляторы пользователя.
5. README обновлён инструкцией запуска.

**Definition of Done:**
```
go run ./cmd/simcastd list
```
печатает РЕАЛЬНЫЙ список симуляторов с машины (UDID + имя + версия iOS + booted/shutdown).
Никаких заглушек — данные из настоящего `idb_companion`.

**Подсказки агенту:**
- `idb.proto` можно скопировать из `~/Developer/ios-bridge/.venv/proto/idb.proto`.
- companion уже стоит: `/opt/homebrew/bin/idb_companion` (v1.1.8).
- Полный список флагов: `idb_companion --help`.
- На этом этапе gRPC ещё НЕ нужен — `--list` это CLI-вывод. gRPC заводим в Phase 1.

---

## Phase 1 — JPEG MVP (видно симулятор на «клиенте»)

**Цель:** самый дешёвый сквозной стрим без WebRTC, чтобы увидеть картинку и проверить ввод.

- gRPC-стабы из `idb.proto` (grpc-go), подключение к `idb_companion --udid <UDID> --grpc-port N`.
- `describe` → размер экрана.
- Кадры: либо `video_stream`, либо периодический `screenshot` → раздаём по WebSocket как JPEG.
- `hid`: реализовать `tap(x,y)` (down+up) и кнопку `Home`.
- Простейший клиент-заглушка (даже веб-страница / маленькое Swift-приложение): рисует кадры,
  по клику шлёт tap с маппингом координат.

**DoD:** на «клиенте» видно живой экран симулятора, клик проходит как тап в симулятор.

---

## Phase 2 — WebRTC (низкая задержка)

- pion: `TrackLocalStaticSample`, H.264 из `video_stream` в трек без энкодинга.
- Signaling (минимальный WSS): offer/answer/ICE.
- Тачи → WebRTC DataChannel (unreliable/unordered).
- Swipe + остальные кнопки в `hid`.

**DoD:** низколатентный H.264-поток на клиента по WebRTC, тачи по DataChannel.

---

## Phase 3 — нативный iPad-клиент (платная часть)

- Swift + GoogleWebRTC, приём трека, рендер, gesture-слой (tap/swipe/long-press/клавиатура).
- Корректный маппинг координат + повороты/letterboxing.

---

## Phase 4 — дистрибуция и удалёнка

- GoReleaser + Homebrew tap (`depends_on idb-companion`, `service` блок).
- ICE по-взрослому: STUN + TURN (coturn), защита signaling.
- Аккаунты/биллинг для клиента.

---

### Что НЕ делать раньше времени
Адаптивный битрейт, свой ScreenCaptureKit-пайплайн, бандлинг companion'а, нотаризация GUI,
реальные устройства — см. «Сознательно отложено» в `ARCHITECTURE.md`.
