# Версии и совместимость

Core 1.1.11 (22 октября 2026): `remnacust-device-speed-v1`, необязательный `deviceBytesPerSecond` и иерархическая очередь скорости. Общий бюджет делится между активными пользователями, затем между их аутентифицированными ключами устройств. Несколько соединений одного ключа не умножают его долю; простаивающие ключи не резервируют канал. Поддержка требуется одновременно в панели, агенте и активном ядре. Копии ключа считаются одной идентичностью. Между нодами сохраняется консервативное разделение пределов; автоматический перенос свободной скорости между нодами не добавлен. Основа обновлена до официального опубликованного Xray v26.10.21; требуется Go 1.27.3 с исправлением проверки CA.

Версия 1.1.9 (20 октября 2026): исправлено ложное отличие listener из-за порядка protobuf-полей и вложенных TypedMessage. Порт, транспорт, параметры безопасности и sniffing остаются защищены от неявной замены. Проверены одинаковые настройки, реальные изменения и живая VLESS-сессия; смена реального listener требует нового порта/тега либо явного обслуживания. Официальная основа не изменена.

Локальная правка 18 октября 2026 (не опубликована): нода использует приватный файловый `internal.sock` для получения конфигурации и служебных webhook. При восстановлении ядро обновляет как прежние abstract-адреса, так и новые адреса `/run/remnacust-xray-<id>/internal.sock`; пользовательские HTTP(S)-webhook и произвольные файловые Unix-адреса не переписываются. Управляющий API переносится на актуальный `api.sock,0600` как для служебного inbound `REMNAWAVE_API_INBOUND` (с сохранением прежнего имени), так и для прямого API Lua-профиля. Пустой direct-listen не превращается в дублирующий listener. Клиентские протоколы этим не меняются. Дополнительно устранены промежуточные `uintptr` в доступе VLESS к TLS/REALITY-буферам и копирование protobuf/mutex в XHTTP XMUX; менеджер сохраняет собственный снимок настроек.

Основа независимых графов профилей описана в [PROFILE-GRAPHS.md](PROFILE-GRAPHS.md).
Это ещё не завершённая интеграция отдельных портов с панелью и нодой.

Рабочие исходники Remnacust Core 1.1.8 основаны строго на опубликованном официальном теге Xray-core **v26.10.21** (`1b1647cd9ddf0abc34b981d4611fc03bcac1ca74`, официальный prerelease). Дополнительные коммиты из `main` не включаются. Баннер версии проверяется по этим же метаданным и не должен заявлять несуществующий набор дополнительных коммитов. Исправления восстановления приватных API включены в 1.1.7. Происхождение и политика обновлений: [REMNACUST-UPSTREAM.json](../xray/REMNACUST-UPSTREAM.json).

В выбранном релизе XDNS использует TCP/UDP; наши прежние поля `type/settings.addr` и имя домена `name` сохраняются для совместимости. DoH, дополнительные `worker`/`tlsConfig`/`quicConfig` XDNS, новый лёгкий стек WireGuard/MASQUE и Noise Lua из последующих коммитов не входят в обновление. WireGuard/MASQUE используют реализацию выбранного официального релиза.

Lua DNS/маршрутизации использует файлы `.lua`; поддержка встроенного `base64:`/`base64+gzip:` из последующих коммитов не включена. MASQUE WARP присутствует в выбранном релизе, дополнительный MASQUE XMUX из `main` не переносится. WARP задаётся в клиентском outbound; обновление сервера не добавляет поддержку в клиентские приложения с другим ядром. Lua маршрутизации заменяет обычные rules/domainStrategy. Наш XERA/XHTTP XMUX остаётся отдельной существующей функцией.

В готовящемся обновлении ноды профиль с Lua сохраняет служебный API через отдельный Unix-сокет `0600`, без добавления обычных правил к скрипту. Сочетание Lua с плагином Torrent Blocker, добавляющим rules, отклоняется до применения. Проверены API статистики и live-смена маршрута Lua по конфигурации настоящего генератора ноды. Это не включает ещё не завершённую поддержку Lua в многопрофильном редакторе отдельных портов.

При live-смене маршрутов `dialerProxy` и DNS используют контекст обслуживающего соединение поколения. Существующие TCP-потоки сохраняют прежнюю цепочку, новые используют подтверждённую конфигурацию; этот сценарий проверен реальным трафиком и Linux `-race`. Полная независимость всех глобальных настроек профилей остаётся незавершённой — текущий компилятор отклоняет несовместимые сочетания, а не игнорирует их.

В рабочем коде native live-apply также меняет DNS, логирование и политики уровней вместе с поколением маршрутов. Счётчики и владелец квот остаются общими; системные счётчики слушателей требуют прежнего режима обслуживания. MASQUE сохраняет контекст принятого туннеля для внутренних IP-потоков. Это не завершает передачу независимых графов из панели в ноду; подробные границы и проверки — в [PROFILE-GRAPHS.md](PROFILE-GRAPHS.md).

При завершении поколения закрываются его DNS-пулы и фоновые задачи; поздний ответ не может запустить их повторно. Проверены локальные HTTP/2 и QUIC, отмена системного DNS и закрытие после последнего старого VLESS-потока. Публичные QUIC-проверки AdGuard в текущем окружении завершаются таймаутом и не считаются пройденными.

Для политик отдельных хостов и тегов, ограничения скорости и отзыва доступа устройств нужны совместимые [панель](https://github.com/lottman/Remnacust-panel) и [агент](https://github.com/lottman/Remnacust-node). Замена только Xray не обновляет API агента и не добавляет ему новые команды.

Исходники olcRTC находятся в `vendor/olcrtc`. Сохраните относительный путь из `xray/go.mod` при сборке. Go-модуль сохраняет имя upstream для совместимости импортов.

Редактор использует строители конфигурации этого ядра. Браузерная проверка не проверяет фактическую доступность сокетов, доменов и сертификатов. После изменения ядра обновите WASM и схему панели, а также проверяемый архив исходников ядра в ноде. Команды приведены в [BUILDING.md](BUILDING.md).


## Registered quota grants (unreleased)

The server-only `remnacust-quota-registrations-v1` protocol durably prepares new credit before a later policy can activate it. Prepared credit cannot carry traffic. Cancellation seals used credit at its in-process charge, or the conservative prewritten charge after restart. An unknown identifier can receive a zero tombstone only with an explicit never-activated cancellation assertion from the panel. Legacy grants cannot be converted into prepared grants.

Enrollment changes the private journal version; it cannot be downgraded. Journal replay validates preparation/activation monotonicity and scale/binding preservation. Node boot, managed-core selection and installer image checks require compatible implementations. No client protocol or subscription field is introduced. `TestRuntimeRegisteredQuotaIsolated` verifies a real VLESS TCP stream, zero-credit preparation, activation, unaffected connections when cancelling different unused credit, and exact used closure in a disposable container. Journal/database rollback detection and full operational recovery remain pending.


### Journal watermarks (unreleased)

`remnacust-quota-watermarks-v1` is the next private journal version. A server policy carries `quotaCheckpoint: { instance, sequence }`; canonical decimal `sequence` is checked against the durable journal and previously persisted checkpoint before mutation. Cold startup also validates the saved policy before public listeners. The checkpoint cannot be removed or lowered after enrollment. The isolated VLESS TCP fixture restores an old journal and verifies startup fails specifically because of its checkpoint, while normal live policy updates preserve the existing stream. Current offline restarts need no panel connection. Known-boundary checks do not reconstruct missing usage or prove simultaneous rollback of every copy without external retained state.

## Служебные ответы API — 1.1.8

Команды `xray api` выводят предупреждения конфигурации в stderr, а служебный ответ — в stdout. Предупреждение об устаревшем `freedom.domainStrategy` больше не ломает чтение JSON нодой при проверке, сохранении и применении профиля. Предупреждения не скрываются. Регрессионный тест запускает настоящие CLI-команды с такой конфигурацией, проверяет JSON, доставку предупреждения и сохранение старого соединения при live-apply. Обычный запуск Xray сохраняет свой прежний вывод логов.
