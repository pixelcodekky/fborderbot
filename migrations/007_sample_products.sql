-- Sample products for development/testing. Added to every page in `pages`,
-- skipping any product name the page already has, so it's safe to re-run.
-- Don't apply this on a production database.
INSERT INTO products (page_id, name, description, price_cents, currency, image_url)
SELECT p.id, s.name, s.description, s.price_cents, 'sgd', s.image_url
FROM pages p
CROSS JOIN (VALUES
    ('Test Sales Item', 'Sample product for the "test sales" keyword',       10000, 'https://picsum.photos/seed/test-sales/600/400'),
    ('Red Dress',       'Cotton summer dress, sizes S-XL',                    4590, 'https://picsum.photos/seed/red-dress/600/400'),
    ('Denim Jacket',    'Classic blue denim jacket, unisex',                  7900, 'https://picsum.photos/seed/denim-jacket/600/400'),
    ('Canvas Tote Bag', 'Reusable tote bag, 38 x 42 cm',                      1500, 'https://picsum.photos/seed/tote-bag/600/400'),
    ('Sneakers',        'White low-top sneakers, EU 36-45',                   8950, 'https://picsum.photos/seed/sneakers/600/400'),
    ('Phone Case',      'Shockproof clear case, iPhone and Samsung models',   1290, 'https://picsum.photos/seed/phone-case/600/400')
) AS s(name, description, price_cents, image_url)
WHERE NOT EXISTS (
    SELECT 1 FROM products x WHERE x.page_id = p.id AND x.name = s.name
);

-- Sample keywords, one per product. "test sales" (seeded by 005) gets linked
-- to its sample product only if it has no product yet.
INSERT INTO trigger_keywords (page_id, keyword, product_id)
SELECT pr.page_id, s.keyword, pr.id
FROM (VALUES
    ('test sales', 'Test Sales Item'),
    ('red dress',  'Red Dress'),
    ('denim',      'Denim Jacket'),
    ('tote',       'Canvas Tote Bag'),
    ('sneakers',   'Sneakers'),
    ('phone case', 'Phone Case')
) AS s(keyword, product_name)
JOIN products pr ON pr.name = s.product_name
ON CONFLICT (page_id, lower(keyword))
    DO UPDATE SET product_id = coalesce(trigger_keywords.product_id, EXCLUDED.product_id);
