# Версии и совместимость

Основа независимых графов профилей описана в [PROFILE-GRAPHS.md](PROFILE-GRAPHS.md).
Это ещё не завершённая интеграция отдельных портов с панелью и нодой.

Рабочие исходники Remnacust Core 1.1.6 основаны строго на опубликованном официальном теге Xray-core **v26.10.10** (`701af60772cda123492f86e383e1fdba066614a9`, официальный prerelease). Дополнительные коммиты из `main` не включаются. Версия форка ещё не собрана и не опубликована. Последний выпущенный Core 1.1.5 сохраняет основу 26.9.30. Происхождение и политика обновлений: [REMNACUST-UPSTREAM.json](../xray/REMNACUST-UPSTREAM.json).

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
