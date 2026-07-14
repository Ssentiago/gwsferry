#!/usr/bin/env python3
"""
Полностью опустошает тестовую почту на Yandex.
Подключается по IMAP (XOAUTH2), проходит по всем папкам,
помечает все письма как \\Deleted, делает EXPUNGE.
"""

import imaplib
import json
import ssl
import sys
import time
import urllib.request
import urllib.parse
from pathlib import Path

# ==========================================
# КОНФИГУРАЦИЯ
# ==========================================

IMAP_HOST = "imap.yandex.ru"
IMAP_PORT = 993

PROJECT_ROOT = Path(__file__).parent


def load_config():
    """Читает gwsferry.toml (парсим вручную — без зависимостей)."""
    config = {}
    with open(PROJECT_ROOT / "gwsferry.toml") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or line.startswith("["):
                continue
            if "=" in line:
                key, _, val = line.partition("=")
                config[key.strip()] = val.strip().strip('"')
    return config


def load_users():
    """Читает yandex_users.json."""
    with open(PROJECT_ROOT / "yandex_users.json") as f:
        data = json.load(f)
    return data.get("users", [])


def exchange_token(client_id, client_secret, org_token, uid):
    """Получает временный токен для IMAP XOAUTH2 через token exchange."""
    url = "https://oauth.yandex.ru/token"
    data = urllib.parse.urlencode({
        "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
        "client_id": client_id,
        "client_secret": client_secret,
        "subject_token": str(uid),
        "subject_token_type": "urn:yandex:params:oauth:token-type:uid",
    }).encode()

    req = urllib.request.Request(url, data=data, method="POST")
    with urllib.request.urlopen(req) as resp:
        result = json.loads(resp.read())
    return result["access_token"]


def get_uid_from_org_api(org_token, org_id, email):
    """Получает numeric uid юзера через Yandex 360 Directory API."""
    url = f"https://api360.yandex.net/directory/v1/org/{org_id}/users?per_page=1000"
    req = urllib.request.Request(url)
    req.add_header("Authorization", f"OAuth {org_token}")

    page = 1
    while True:
        paged_url = f"{url}&page={page}"
        req = urllib.request.Request(paged_url)
        req.add_header("Authorization", f"OAuth {org_token}")
        with urllib.request.urlopen(req) as resp:
            data = json.loads(resp.read())

        for u in data.get("users", []):
            if u.get("email") == email and u.get("isEnabled") and not u.get("isDismissed"):
                return u["id"]

        if page >= data.get("pages", 1):
            break
        page += 1

    return None


def imap_connect(email, access_token):
    """Подключается к IMAP через XOAUTH2."""
    context = ssl.create_default_context()
    conn = imaplib.IMAP4_SSL(IMAP_HOST, IMAP_PORT, ssl_context=context)

    auth_string = f"user={email}\x01auth=Bearer {access_token}\x01\x01"
    conn.authenticate("XOAUTH2", lambda x: auth_string.encode())

    return conn


def parse_list_response(line):
    """Парсит строку из IMAP LIST: flags, delimiter, name.
    
    Формат: (flags) delimiter name
    Пример: (\\HasNoChildren \\Unmarked \\Drafts) "|" Drafts
    Имя папки — ВСЕГДА последний токен после разделителя.
    """
    line = line.decode() if isinstance(line, bytes) else line
    
    # Убираем flags (всё до первого ')')
    close_paren = line.find(')')
    if close_paren == -1:
        return line.split()[-1] if line.split() else ""
    
    after_flags = line[close_paren + 1:].strip()
    
    # after_flags: '"|" Drafts' или 'NIL INBOX'
    # Разделитель — первый токен (в кавычках или NIL)
    # Имя — всё после разделителя
    parts = after_flags.split(None, 1)  # split на max 2 части
    if len(parts) < 2:
        return ""
    
    return parts[1]  # имя папки


def wipe_mailbox(conn):
    """Проходит по всем папкам, помечает все письма \\Deleted, делает EXPUNGE.
    
    Batch STORE: помечает \\Deleted чанками по 1000 ID за раз вместо по одному.
    13к писем = 14 запросов вместо 13000.
    """
    CHUNK_SIZE = 1000

    typ, folders = conn.list()
    if typ != "OK":
        print(f"  Ошибка получения списка папок: {folders}")
        return

    total_deleted = 0
    for folder_info in folders:
        folder_name = parse_list_response(folder_info)
        if not folder_name:
            continue

        print(f"  Папка: {folder_name!r}", end=" ... ", flush=True)

        try:
            typ, data = conn.select(folder_name, readonly=False)
        except Exception as e:
            print(f"пропуск ({e})")
            continue
        if typ != "OK":
            print(f"пропуск (select failed)")
            continue

        msg_count = int(data[0])
        if msg_count == 0:
            print("пусто")
            continue

        typ, msg_ids = conn.search(None, "ALL")
        if typ != "OK" or not msg_ids[0]:
            print("пусто")
            continue

        ids = msg_ids[0].split()
        print(f"{len(ids)} писем", end=" ... ", flush=True)

        # Batch STORE — чанками по CHUNK_SIZE
        for i in range(0, len(ids), CHUNK_SIZE):
            chunk = ids[i:i + CHUNK_SIZE]
            id_range = b",".join(chunk)
            typ, store_data = conn.store(id_range, "+FLAGS", "\\Deleted")
            if typ != "OK":
                print(f"ошибка STORE на чанке {i}: {store_data}")
                break

        typ, expunge_data = conn.expunge()
        deleted = len(expunge_data) if expunge_data else 0
        total_deleted += deleted
        print(f"удалено {deleted}")

    return total_deleted


def main():
    print("=" * 50)
    print("ОПУСТОШЕНИЕ ТЕСТОВОЙ ПОЧТЫ YANDEX")
    print("=" * 50)

    config = load_config()
    users = load_users()

    if not users:
        print("Нет юзеров в yandex_users.json")
        sys.exit(1)

    print(f"Юзеры: {users}")
    print(f"Org ID: {config.get('org_id')}")
    print()

    for email in users:
        print(f"--- {email} ---")

        # 1. Получаем uid через Directory API
        print("  Получаю uid из Directory API...", end=" ", flush=True)
        uid = get_uid_from_org_api(
            config["oauth_token"], config["org_id"], email
        )
        if not uid:
            print(f"ОШИБКА: юзер {email} не найден в организации")
            continue
        print(f"uid={uid}")

        # 2. Обмениваем токен
        print("  Обмениваю токен...", end=" ", flush=True)
        access_token = exchange_token(
            config["client_id"], config["client_secret"],
            config["oauth_token"], uid
        )
        print("OK")

        # 3. Подключаемся к IMAP
        print(f"  Подключаюсь к {IMAP_HOST}:{IMAP_PORT}...", end=" ", flush=True)
        conn = imap_connect(email, access_token)
        print("OK")

        # 4. Опустошаем
        print("  Опустошаю почту...")
        deleted = wipe_mailbox(conn)

        # 5. Закрываем
        conn.logout()
        print(f"  Итого удалено: {deleted} писем")
        print()

    print("=" * 50)
    print("ГОТОВО")
    print("=" * 50)


if __name__ == "__main__":
    main()
