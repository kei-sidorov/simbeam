# Исследование: латентность видео-пайплайна (2026-06-08)

Журнал замеров и фактов. **Без выводов и рекомендаций** — это справочник, чтобы при
будущей оптимизации не повторять уже пройденные шаги. Все измерительные правки кода
после сессии откачены (см. «Инструменты» ниже).

## Конфигурация на момент замера

- Локальный прогон: `make run-remote BROKER=ws://localhost:9000/ws` (демон) + `go run ./cmd/simcast-signal` (брокер на :9000) + браузерный debug-клиент `http://localhost:8080/`, путь RTC (WebRTC).
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

## Инструменты (после сессии удалены)

- `web/debug/timer.html` — rAF-таймер для метода «двух часов».
- Оверлей `getStats()` в `web/debug/index.html`.
- Счётчики `framesIn/framesOut` в `internal/encoder/ffmpeg.go` и тайминг RPC в `internal/idb/client.go`.

Все перечисленные правки откачены к состоянию до сессии; в репозитории их нет.
