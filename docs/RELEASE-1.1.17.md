# Core 1.1.17

Основа — официальный Xray v26.11.9, commit 676eaf66f8f05d4d558ee7a74b4736623d3feeb9. Перенесены изменения runtime и зависимостей из выпуска: REALITY/uTLS с Chrome 155, mux, очистка неудачного inbound, Freedom, TUN, WireGuard, geodata и XDRIVE. Сохранены расширения хостов, устройств, квот, автономности и Xera HTTP.

XDRIVE по умолчанию сохраняет сегментные объявления для старых узлов; `xdriveSettings.useSlots: true` включает новые слоты для совместимых принимающих сторон. Custom schema/WASM поддерживают параметр; pristine default использует upstream-поведение. Конфигурации редактора и native Core проверяются отдельно.

Исправление декомпрессии TLS-сертификатов теперь входит в выбранный официальный uTLS a13c59ffe98bb01b2ca14143eb2e887664b1f138. Локальные регрессионные проверки форматов, длины, обрезанных потоков и хвостовых данных сохранены; дополнительный патч к исходникам uTLS не применяется.

MPTCP остаётся выключенным по умолчанию; явный tcpMptcp сохранён. Это не закрывает CVE-2026-98432 в ядре Linux. Проверяйте исправленный пакет у поставщика ОС и отдельно планируйте загрузку обновлённого ядра. Статус Ubuntu на 9 ноября 2026 года: https://ubuntu.com/security/CVE-2026-98432.

## English

Based on official Xray v26.11.9. Runtime and dependency updates retain Remnacust quota, device, host-policy, offline and Xera extensions. Legacy XDRIVE segments remain the default; useSlots explicitly enables modern slot announcements for compatible peers. Both editor flavors are rebuilt. Certificate decompression boundary fixes are included in official uTLS and retain native codec regression tests. MPTCP remains disabled by default; kernel remediation for CVE-2026-98432 is a separate OS operation.
