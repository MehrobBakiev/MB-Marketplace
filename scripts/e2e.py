#!/usr/bin/env python3
"""HTTP + PostgreSQL demonstration. Only Python's standard library is required."""
import concurrent.futures
import datetime as dt
import json
import os
from pathlib import Path
import subprocess
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
os.chdir(ROOT)
# Compose also reads .env. Read its simple KEY=value settings for this client.
settings = {}
if (ROOT / '.env').exists():
    for line in (ROOT / '.env').read_text().splitlines():
        if line.strip() and not line.lstrip().startswith('#') and '=' in line:
            key, value = line.split('=', 1)
            settings[key.strip()] = value.strip().strip('\'"')
settings.update(os.environ)
BASE = settings.get('API_URL', 'http://localhost:' + settings.get('HTTP_PORT', '8080'))
SUFFIX = uuid.uuid4().hex[:10]
PASSWORD = 'Demo-password-123!'


def call(method, path, body=None, token=None, expected=200, code=None):
    data = None if body is None else json.dumps(body).encode()
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(req, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    raw = response.read()
    result = json.loads(raw) if raw else None
    assert response.status == expected, (method, path, response.status, result)
    uuid.UUID(response.headers['X-Request-Id'])
    if code:
        assert result['error_code'] == code, result
    print(f'{method:6} {path:40} -> {response.status}' + (f' {code}' if code else ''))
    return result


def account(role):
    email = f'demo-{role.lower()}-{uuid.uuid4().hex[:12]}@example.com'
    call('POST', '/auth/register', {'email': email, 'password': PASSWORD, 'role': role}, expected=201)
    return call('POST', '/auth/login', {'email': email, 'password': PASSWORD})


def sql(query):
    result = subprocess.run(
        ['docker', 'compose', 'exec', '-T', 'postgres', 'sh', '-c',
         'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
        input=query, text=True, capture_output=True, check=True,
    )
    print('\nSQL>', query.strip())
    print(result.stdout)
    return result.stdout


def product(token, stock=10):
    return call('POST', '/products', {'name': 'Книга ' + SUFFIX, 'description': None,
                'price': 100, 'stock': stock, 'category': 'demo-' + SUFFIX, 'status': 'ACTIVE'}, token, 201)


def order_body(product_id, quantity):
    return {'items': [{'product_id': product_id, 'quantity': quantity}]}


def main():
    call('GET', '/health')
    call('GET', '/products', expected=401, code='TOKEN_INVALID')
    seller, buyer, stranger = account('SELLER'), account('USER'), account('USER')
    admin = call('POST', '/auth/login', {
        'email': settings.get('ADMIN_EMAIL', 'admin@example.com'),
        'password': settings.get('ADMIN_PASSWORD', 'Admin-local-123!'),
    })
    st, bt = seller['access_token'], buyer['access_token']
    call('POST', '/products', {'name': 'x', 'price': 1, 'stock': 1,
         'category': 'x', 'status': 'ACTIVE'}, bt, 403, 'ACCESS_DENIED')
    p = product(st)
    call('POST', '/products', {'name': '', 'price': 0, 'stock': -1,
         'category': '', 'status': 'BAD'}, st, 400, 'VALIDATION_ERROR')
    now = dt.datetime.now(dt.timezone.utc)
    promo = call('POST', '/promo-codes', {'code': 'SAVE_' + SUFFIX.upper(),
        'discount_type': 'PERCENTAGE', 'discount_value': 10, 'min_order_amount': 150,
        'max_uses': 3, 'valid_from': (now - dt.timedelta(hours=1)).isoformat(),
        'valid_until': (now + dt.timedelta(hours=1)).isoformat()}, st, 201)
    body = order_body(p['id'], 2)
    body['promo_code'] = promo['code']
    order = call('POST', '/orders', body, bt, 201)
    path = '/orders/' + str(order['id'])
    assert order['total_amount'] == 180 and order['discount_amount'] == 20
    assert call('GET', '/products/' + str(p['id']), token=bt)['stock'] == 8
    sql(f"SELECT id, name, price, stock, status FROM products WHERE id={p['id']};\n"
        f"SELECT id, user_id, status, total_amount, discount_amount, promo_code_id FROM orders WHERE id={order['id']};\n"
        f"SELECT product_id, quantity, price_at_order FROM order_items WHERE order_id={order['id']};\n"
        f"SELECT code, current_uses, max_uses FROM promo_codes WHERE id={promo['id']};\n"
        f"SELECT user_id, operation_type, created_at FROM user_operations WHERE user_id={buyer['user']['id']};")
    # Change the catalogue price while retaining its current available stock.
    call('PUT', '/products/' + str(p['id']), {'name': p['name'], 'price': 120,
         'stock': 8, 'category': p['category'], 'status': 'ACTIVE'}, st)
    snapshot = call('GET', path, token=bt)
    assert snapshot['items'][0]['price_at_order'] == 100 and snapshot['total_amount'] == 180
    call('POST', '/orders', body, bt, 429, 'ORDER_LIMIT_EXCEEDED')
    call('GET', path, token=stranger['access_token'], expected=403, code='ORDER_OWNERSHIP_VIOLATION')
    call('PUT', path, order_body(p['id'], 11), bt, 409, 'INSUFFICIENT_STOCK')
    assert call('GET', '/products/' + str(p['id']), token=bt)['stock'] == 8
    changed = call('PUT', path, order_body(p['id'], 1), bt)
    assert changed['promo_code_id'] is None and changed['discount_amount'] == 0 and changed['total_amount'] == 120
    call('PUT', path, order_body(p['id'], 2), bt, 429, 'ORDER_LIMIT_EXCEEDED')
    call('POST', path + '/status', {'status': 'SHIPPED'}, admin['access_token'], 409, 'INVALID_STATE_TRANSITION')
    call('POST', path + '/status', {'status': 'PAYMENT_PENDING'}, admin['access_token'])
    call('PUT', path, order_body(p['id'], 2), bt, 409, 'INVALID_STATE_TRANSITION')
    call('POST', path + '/cancel', token=bt)
    call('POST', path + '/cancel', token=bt, expected=409, code='INVALID_STATE_TRANSITION')
    assert call('GET', '/products/' + str(p['id']), token=bt)['stock'] == 10
    rotated = call('POST', '/auth/refresh', {'refresh_token': buyer['refresh_token']})
    call('POST', '/auth/refresh', {'refresh_token': buyer['refresh_token']}, expected=401, code='REFRESH_TOKEN_INVALID')
    call('GET', '/products?category=demo-' + SUFFIX + '&page=0&size=1', token=rotated['access_token'])
    call('DELETE', '/products/' + str(p['id']), token=st, expected=204)
    assert call('GET', '/products/' + str(p['id']), token=bt)['status'] == 'ARCHIVED'
    # Competing HTTP requests for the last item: exactly one can succeed.
    last = product(st, stock=1)
    competitors = [account('USER'), account('USER')]
    def compete(user):
        request = urllib.request.Request(BASE + '/orders', data=json.dumps(order_body(last['id'], 1)).encode(),
            headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + user['access_token']}, method='POST')
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        return response.status, json.loads(response.read())
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(compete, competitors))
    assert sorted(status for status, _ in results) == [201, 409], results
    assert [body['error_code'] for status, body in results if status == 409] == ['INSUFFICIENT_STOCK']
    print('Два конкурентных заказа: 201 + 409 INSUFFICIENT_STOCK')
    sql(f"SELECT id, stock, status FROM products WHERE id IN ({p['id']},{last['id']});\n"
        f"SELECT id, status, total_amount, discount_amount, promo_code_id FROM orders WHERE id={order['id']};\n"
        f"SELECT code, current_uses FROM promo_codes WHERE id={promo['id']};")
    print('Все E2E-проверки пройдены. Демонстрационные записи сохранены в БД.')


if __name__ == '__main__':
    main()
