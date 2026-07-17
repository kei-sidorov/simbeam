# Исследование: латентность видео-пайплайна (2026-06-08)

Журнал замеров и фактов. **Без выводов и рекомендаций** — это справочник, чтобы при
будущей оптимизации не повторять уже пройденные шаги. Все измерительные правки кода
после сессии откачены (см. «Инструменты» ниже).

## Конфигурация на момент замера

- Локальный прогон: `make run-remote BROKER=ws://localhost:9000/ws` (демон) + `go run ./cmd/simbeam-signal` (брокер на :9000) + браузерный debug-клиент `http://localhost:8080/`, путь RTC (WebRTC).
- Симулятор: iPhone 17 Pro (iOS 26.1), `rtcFPS = 15` (`internal/server/rtc.go`), интервал поллинга 66.67 мс.
- Пайплайн: idb `screenshot` (PNG, поллинг в `ScreenshotStream`) → ffmpeg `h264_videotoolbox` → NAL/AU сборка → pion `TrackLocalStaticSample` → браузерный `<video>`.
- Аргументы ffmpeg (`internal/encoder/ffmpeg.go`): `-fflags nobuffer -flags low_delay -analyzeduration 0 -f image2pipe -vcodec png -framerate 15 -i pipe:0 -an -vf scale=iw/2:ih/2 -c:v h264_videotoolbox -realtime 1 -profile:v baseline -g 30 -b:v 8M -pix_fmt yuv420p -flush_packets 1 -max_delay 0 -f h264 pipe:1`. (`-flags low_delay` здесь стоит **до `-i`**, т.е. на декодере входа.)

## Методика

1. **Glass-to-glass («двое часов в одном кадре»)**: страница с миллисекундным таймером (`requestAnimationFrame`) открыта в Safari симулятора через `xcrun simctl openurl booted http://localhost:8080/timer.html`; одним `screencapture` снимаются одновременно симулятор и браузерный `<video>`; задержка = (время в симуляторе − время в браузере).
2. **Приёмная сторона**: оверлей в debug-странице опрашивает `pc.getStats()` (`inbound-rtp`, video) каждые 500 мс.
3. **Send-side**: счётчики в `encoder.Encode` (PNG записано в stdin ffmpeg = `framesIn` vs AU выдано = `framesOut`; разница = кадры «в полёте» в ffmpeg/энкодере) и таймер длительности `Screenshot` RPC в `idb.ScreenshotStream`.

## Замеры — baseline

Glass-to-glass (сим − браузер), две серии скриншотов:

| сим | браузер | Δ |
|---|---|---|
| 28.550 | 27.950 | 0.600 с |
| 27.583 | 26.950 | 0.633 с |
| 26.633 | 26.017 | 0.616 с |
| 25.283 | 24.683 | 0.600 с |
| 135.632 | 134.965 | 0.667 с |
| 132.314 | 131.631 | 0.683 с |
| 129.064 | 128.364 | 0.700 с |

Диапазон ≈ **0.60–0.70 с**, разброс малый (±15 мс).

Покадровый разбор:

| Стадия | Значение |
|---|---|
| idb `Screenshot` RPC, среднее | **71.5–73.9 мс/кадр** (в одном окне проседало до 30–38 мс); интервал поллинга 66.67 мс |
| ffmpeg/videotoolbox in-flight | **7 кадров, стабильно ≈ 466 мс @ 15fps** |
| браузер `jitterBufferDelay` | **69–72 мс/кадр** |
| браузер `totalDecodeTime/framesDecoded` | **~2.0 мс/кадр** |
| браузер `framesPerSecond` | 15 |
| браузер `framesDropped` | 0 |
| браузер `packetsLost` | 1 |
| браузер `jitter` | 20–27 мс |

## Эксперимент 1 — `-flags +low_delay` на энкодере (выход)

Перенесли `-flags +low_delay` в выходную секцию (после `-c:v h264_videotoolbox`), чтобы выставить `AV_CODEC_FLAG_LOW_DELAY` → `EnableLowLatencyRateControl`. Битрейт задан явно (`-b:v 8M`), требование режима выполнено.

Результат (логи `encoder`):

| in-flight | буфер | glass-to-glass |
|---|---|---|
| **20–21 кадр** | ≈ 1333–1400 мс | ≈ 2 с |

То есть стало **хуже** (было 7 кадров). Изменение откачено.

## Факты из исходников (сверено в эту сессию)

**ffmpeg `libavcodec/videotoolboxenc.c`:**
- Low-latency режим (`EnableLowLatencyRateControl`, комментарий «eliminate frame reordering, follow a one-in-one-out encoding mode») включается при `(avctx->flags & AV_CODEC_FLAG_LOW_DELAY) && H264 && явный bit_rate`.
- `kVTCompressionPropertyKey_MaxFrameDelayCount` **не выставляется нигде** (символ в файле отсутствует).
- AVOptions энкодера: `allow_sw, require_sw, realtime, frames_before, frames_after, prio_speed, power_efficient, spatialaq, max_ref_frames, profile, level, coder, a53cc, constant_bit_rate, max_slice_bytes`.

**idb (facebook/idb) — путь скриншота:** gRPC `screenshot` → `ScreenshotMethodHandler` → `take_screenshot` → `FBSimulatorScreenshotCommands.takeScreenshotAsync` → `connectToFramebuffer()` (`SimDeviceIOPortInterface` → `SimDisplayIOSurfaceRenderable` → `framebufferSurface`/`ioSurface`, `registerCallback(ioSurfacesChangeCallback:)`) → `FBSimulatorImage`: `CIImage(ioSurface:)` → `createCGImage` → PNG/JPEG через ImageIO. Дедуп кадров по `surface.seed` в `FBSurfaceImageGenerator.availableImage()`.

**idb `FBSimulatorVideoStream.m`:** кодирует через `VTCompressionSession` на `CVPixelBuffer`/IOSurface; выставляет `kVTCompressionPropertyKey_RealTime`, `MaxKeyFrameIntervalDuration`, `ProfileLevel`; форсит keyframe через `kVTEncodeFrameOptionKey_ForceKeyFrame` (при `frameNumber == 0 || forceKeyFrame`); есть `MaxKeyFrameInterval : @360` в одном из путей.

**idb `video_stream` (замер 2026-06-02, companion 1.1.8):** фиксированный GOP ~10 с, IDR не вставляется на смену сцены; `VideoStreamRequest.Start` отдаёт только `fps`, `format`, `compression_quality`, `scale_factor`.

## Сессия 2026-07-17 — `-threads 1` на входном декодере

Гипотеза на входе: PNG-декодер frame-threaded (`ffmpeg -h decoder=png` → "Threading
capabilities: frame"), `-threads` в argv не задан → auto (12 ядер на этой машине);
frame-threaded декодер держит ~N−1 кадров конвейерной задержки → ожидание −350…−450 мс.

Конфигурация: та же, что в замере 2026-06-08 (симулятор iPhone 17 Pro / iOS 26.1,
15 fps, scale 0.5, 8 Мбит/с), ffmpeg 7.1.1, тот же Mac (12 ядер). Прогон через
локальный брокер `:9000`, debug-клиент в Chrome. Инструментация вернулась в репо
насовсем: счётчики in-flight за env-флагом `SIMBEAM_ENCODER_STATS=1`, страница
`web/debug/timer.html` — в репо (см. «Инструменты»).

### Baseline сегодня (до изменения)

| Метрика | Значение |
|---|---|
| glass-to-glass (3 замера) | 0.484 / 0.470 / 0.483 с — **≈0.48 с** (в июне было 0.60–0.70) |
| in-flight счётчик | **константно 23** после старта |
| jitter (браузер) | 4 мс |
| jitterBufferDelay | 29.2 мс/кадр |
| decode | 1.3 мс/кадр |

**Счётчик in-flight ≠ задержка.** Первая секунда: in=14, out=0; к t≈2 c разница
фиксируется на 23 и больше не меняется (rate 15/s в обе стороны). По закону Литтла
23 кадра @15 fps дали бы 1.53 с транзита — glass-to-glass же 0.48 с. Значит бо́льшая
часть константы — кадры, «проглоченные» при старте сессии (заполнение конвейера +
инициализация VT, часть кадров не выходит вовсе) — вечный офсет в разнице in−out, а
реальный устоявшийся in-flight ≈ 5–7 кадров. Июньская интерпретация «7 кадров ≈ 466 мс
внутри ffmpeg», по-видимому, страдала тем же артефактом.

### После `-threads 1` (input-секция, до `-i pipe:0`)

| Метрика | Значение |
|---|---|
| glass-to-glass (3 замера) | 0.433 / 0.484 / 0.500 с — **≈0.47 с, эффекта нет** |
| in-flight счётчик | константно 52 (артефакт старта ещё больше: out=0 первые ~3.5 с) |
| jitter / jbd | 9 мс / 28.6 мс/кадр — в пределах шума |
| fps / dropped / lost | 15 / 0 / 0 — деградации throughput нет |

### Почему эффекта нет: изолированный бенч ffmpeg

Стенд (`ffbench`, вне репо): один и тот же ретина-PNG с симулятора подаётся в тот же
argv ровно 15 fps, меряется PNG#i(записан) → AU#i(вышел), медиана по устоявшейся
части, 120 кадров на вариант:

| Вариант | p50 |
|---|---|
| auto-threads + `-flags low_delay` (наш argv без `-threads`) | 3337 мс |
| `-threads 1` + low_delay (наш argv сейчас) | 3337 мс |
| auto-threads, **без** low_delay | **4135 мс** |
| `-threads 1`, без low_delay | 3337 мс |

- Задержка frame-threading **реальна**: +798 мс ≈ 12 кадров ≈ N−1 при auto-threads —
  но только когда `AV_CODEC_FLAG_LOW_DELAY` НЕ стоит на декодере.
- В нашем argv `-flags low_delay` стоит в input-секции **с самого начала** (см.
  «Конфигурация» от 2026-06-08) — он уже нейтрализовал frame-thread-задержку декодера.
  Поэтому `-threads 1` на живом пайплайне ничего не меняет.
- Абсолютные ~3.3 с бенча — артефакт стенда (вход ровно 15 fps → бэклог старта не
  рассасывается; в живом пайплайне Screenshot RPC даёт ~13.7 fps и очередь тает).
  Сигнал — только относительная разница вариантов.

`-threads 1` оставлен в argv как страховка (декодер останется latency-free, даже если
строку с `-flags low_delay` когда-нибудь тронут) и минус 11 холостых тредов декодера.
Ожидание «−350…−450 мс» не подтвердилось: этот резерв в пайплайне отсутствует, сегодняшний
glass-to-glass и без изменения ≈0.48 с (июньские 0.60–0.70 с не воспроизводятся, разница
не атрибутирована — вероятно, машина/окружение).

## Сессия 2026-07-17 — честный `Frame.Duration` вместо фиксированного 1/fps

До: каждый AU получал `Duration = 1/fps` (66.7 мс), при реальном интервале выдачи
~72–74 мс (Screenshot RPC дороже тикера) — RTP-часы шли ~на 9% медленнее реального
времени. После: `frameTimer` в `encoder.Encode` меряет интервал между выдачами
соседних AU по монотонным часам; первый кадр — номинальный 1/fps; пол — 1 мс (чтобы
на стартовом бёрсте RTP-таймстампы оставались строго монотонными); свежий feed =
свежий `Encode` = свежий таймер (интервал не тянется через смену качества/реконнект).
Маппинг лагает на кадр (ts-дельта N+1 несёт измеренный интервал N−1→N — pion двигает
часы на Duration *текущего* сэмпла): без задержки отправки это неустранимо.

| Метрика | до (шаг 1) | после |
|---|---|---|
| jitter | 4–9 мс | 7–10 мс (первые ~20 c после коннекта — до 17 мс, стартовый бёрст в EWMA) |
| jitterBufferDelay | 28.6–29.2 мс/кадр | **24.4–26.9 мс/кадр** |
| glass-to-glass | 0.433/0.484/0.500 | 0.500/0.534 — в пределах шума |
| fps (receiver-computed) | 15 | 14–16 (честные часы обнажают реальный темп ~13.7) |
| dropped / lost | 0 / 0 | 0 / 0 |

Итог: jbd −3…−5 мс/кадр, остальное в шуме. Ожидание «умеренный выигрыш ~десятки мс»
подтвердилось по нижней границе: приёмник сегодня и так держит jbd ~25–29 мс, а не
~70 мс, как в июньском замере — сжимать почти нечего (см. шаг «playout-delay»).

## Сессия 2026-07-17 — RTP-расширение `playout-delay` (min=max=0) на sender

Реализация (`internal/rtc/peer.go`): явный `MediaEngine` (`RegisterDefaultCodecs` +
`RegisterHeaderExtension(playout-delay, video)`) вместо дефолтного
`webrtc.NewPeerConnection`; дефолтные интерсепторы сохранены
(`RegisterDefaultInterceptors`). Готового playout-delay в `pion/interceptor` v0.1.45
нет (проверено: cc/gcc/nack/report/twcc/…) — написан свой: на `BindLocalStream` берёт
согласованный id расширения из `StreamInfo.RTPHeaderExtensions` и штампует три нулевых
байта (12 бит min + 12 бит max, единицы 10 мс) в каждый исходящий видео-RTP-пакет;
если клиент расширение не согласовал (или стрим не видео) — writer не трогается.
Offer браузерного клиента объявляет `extmap:5 playout-delay` (проверено live);
answer демона теперь его принимает.

| Метрика | до (шаг 2) | после |
|---|---|---|
| jitterBufferDelay | 24.4–26.9 мс/кадр | **0.2 мс/кадр** |
| jitterBufferTargetDelay (то, что приёмник хотел бы) | — | 22–26 мс/кадр — игнорируется благодаря расширению |
| jitter | 7–10 мс | 9–13 мс (шум) |
| glass-to-glass | 0.500/0.534 | **0.383 / 0.417 / 0.451 (≈0.42 с)** |
| dropped / lost | 0 / 0 | 0 / 0 |

**ОТКАЧЕНО 2026-07-17 вечером (v0.6.1): чёрный экран на iOS-клиенте.** После релиза
v0.6.0 SimBeam iOS показывал вечно чёрный видеотрек при живой сессии (recent через
bulk работал, список работал, консоль клиента чистая, демон стримил 15 AU/с без
ошибок). A/B по версии демона: 0.5.0 — живое видео; текущий код без playout-delay —
живое видео; с playout-delay — чернота. Виновник — именно `playout-delay 0/0`:
libwebrtc в iOS-клиенте **объявляет** расширение в offer (согласован ext id 5), но
при min=max=0 не рендерит ни кадра (у свежего Chrome — работает). Вывод: guard «шли
только если согласовано» недостаточен — объявить ≠ пережить. Вернуть можно только
после обновления WebRTC в iOS-приложении и проверки на нём, либо через явную
capability от клиента по control-каналу (не по SDP). Цифры выше (jbd → 0.2 мс,
−50…−80 мс g2g) остаются валидными для браузерного клиента и как ориентир будущего
возврата.

Итог дня после отката (шаги 1–2 остались): RTP-часы честные, jbd 29→25 мс/кадр;
glass-to-glass ≈0.48 → ≈0.47–0.50 с. Оставшийся бюджет ≈0.42 с сидит в захвате
(Screenshot RPC ~72–74 мс + поллинг), ffmpeg/VT (реальный in-flight ~5–7 кадров при
15 fps — их не видно счётчику из-за стартового артефакта, см. выше) и рендере. Дальше
без смены архитектуры захвата (video_stream H264 / Phase 2) жать почти нечего.

## Сессия 2026-07-17 — быстрый пробник латентности `video_stream` (оценка Phase 2 без разработки)

Вопрос: сколько выиграет переход захвата на companion `video_stream` (Phase 2), не
строя его. Метод — «wall-clock в кадре»: страница показывает `Date.now()` (mod 100 c,
`SS.mmm`; часы симулятора = часы Mac, синхронизация не нужна); одноразовый Go-пробник
(~100 строк, жил в `probe_tmp/`, удалён) спавнит companion, стартует
`video_stream(H264, fps=15)`, пишет elementary stream в ffmpeg → PNG на кадр; mtime
PNG = момент «кадр декодирован на Mac»; латентность = mtime − цифры в кадре.

Замер (companion 1.1.8, iPhone 17 Pro / iOS 26.1, ретина без скейла):

| Прогон (по первому чистому IDR-кадру) | в кадре | mtime | латентность |
|---|---|---|---|
| 1 | 41.965 | 42.225 | 260 мс |
| 2 | 48.197 | 48.461 | 264 мс |
| 3 | 64.680 | 64.936 | 256 мс |

**`video_stream`: захват → декодированный кадр на Mac ≈ 260 мс** (внутри ~20–40 мс —
ffmpeg-декод+scale+запись PNG, т.е. «по проводу» ~220–240 мс). Поток идёт стабильно
~14 payload/с все 15 с (~70 мс интервал, payload ≈ кадр).

Сравнение: текущий путь (poll `Screenshot` + свой ffmpeg) на сегодняшних замерах —
g2g ≈ 0.42 с, из них send-side ≈ 0.38–0.40 с. То есть Phase 2 обещает примерно
**−150 мс** (g2g ≈ 0.27–0.30 с) плюс потенциально больший fps (нет сериализации
72-мс RPC). Не серебряная пуля.

Попутно подтверждены известные грабли `video_stream` (№34-38, замер 2026-06-02):
- ffmpeg смог синхронизироваться только на **втором** IDR — ~10 c от старта потока
  (тот самый фиксированный GOP ≈10 с). Подключение клиента к идущему потоку = до 10 с
  чёрного экрана без форс-кейфрейма; лечится только рестартом потока на каждый join
  (так делает ios-bridge — IDR форсится при `frameNumber == 0`).
- Ref-структура потока (до 9 reference frames) валит декодер ffmpeg mid-GOP
  («number of reference frames exceeds max», «co located POCs unavailable») — кадры
  после IDR деградируют; libwebrtc может переваривать иначе, не проверялось.
- `MJPEG`-формат `video_stream` не отдал ни одного payload за 15 c (fps=15,
  quality=0.7, scale 1.0) — не копали.

## Инструменты (после сессии 2026-06-08 удалены; 2026-07-17 возвращены насовсем)

- `web/debug/timer.html` — rAF-таймер для метода «двух часов». **С 2026-07-17 в репо.**
- `web/debug/wallclock.html` — wall-clock-таймер (`Date.now()` mod 100 c) для замеров
  «латентность = mtime файла кадра − цифры в кадре» без второго окна. **С 2026-07-17 в репо.**
- Оверлей `getStats()` в `web/debug/index.html` — в репо нет; тот же срез снимается
  из консоли браузера опросом `pc.getStats()` (inbound-rtp, kind=video).
- Счётчики `framesIn/framesOut` в `internal/encoder/ffmpeg.go` — **с 2026-07-17 в репо
  за env-флагом `SIMBEAM_ENCODER_STATS=1`** (лог `encoder: stats …` раз в секунду);
  помни про стартовый артефакт в константе (см. секцию 2026-07-17). Тайминг RPC в
  `internal/idb/client.go` — в репо нет.
