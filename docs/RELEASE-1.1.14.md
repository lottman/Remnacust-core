# Core 1.1.14

Основа остаётся опубликованным Xray v26.10.31.

- Исправлено восстановление сохранённой политики после удаления истёкших грантов из журнала: действующий кредит сохраняется, истёкший не выдаётся повторно. Для полного offline restart нужен Node 1.1.23 и сохранённое приватное состояние.
- Повторный активный MUX ID отклоняется без замены владельца сессии и потери её quota lease.
- Xera ограничивает ресурсы запросов и пулов, освобождает бюджет незавершённого packet-up при отмене и ограничивает чтение одного тела 300 секундами. Streaming и long polling не получают этот общий таймаут.
- Обновлены HTTP/2-настройки и профиль Xera. Новый профиль требует согласованного обновления клиента и сервера; прежние параметры `xPaddingObfsMode`, `noGRPCHeader`, `noSSEHeader` и `xPaddingMethod: "repeat-x"` отклоняются.
- Brotli обновлён до 1.2.7, compress — до 1.20.2. Исправления чтения сжатых сертификатов перенесены в совместимую закреплённую основу uTLS без смены QUIC API.

Пересоберите схему и WASM редактора из Core 1.1.14 и поставляйте их вместе с панелью. Суточная автономность требует достаточного запаса срока и локального кредита; календарный сброс без панели не пополняет квоту. Проверки виртуального времени не заменяют реальный суточный сетевой прогон.

---

The base remains published Xray v26.10.31.

- Fixes persisted policy recovery after expired grants are removed from the journal: valid credit remains usable and expired credit is not reissued. Complete offline restart requires Node 1.1.23 and preserved private state.
- Rejects a duplicate active MUX ID without replacing the session owner or losing its quota lease.
- Bounds Xera request and pool resources, releases unfinished packet-up admission on cancellation and limits each body read to 300 seconds. Streaming and long polling do not receive this general timeout.
- Updates HTTP/2 settings and the Xera profile. The new profile requires coordinated client and server updates; the former `xPaddingObfsMode`, `noGRPCHeader`, `noSSEHeader` and `xPaddingMethod: "repeat-x"` settings are rejected.
- Updates Brotli to 1.2.7 and compress to 1.20.2. Compressed certificate read fixes are backported to the compatible pinned uTLS base without changing its QUIC API.

Rebuild the editor schema and WASM from Core 1.1.14 and ship them together with the panel. A day of offline operation requires sufficient validity time and local credit; a calendar reset without the panel does not replenish quota. Virtual time checks do not replace a real day-long network run.
