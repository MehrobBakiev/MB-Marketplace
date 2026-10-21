-- PostgreSQL does not create indexes on the referencing side of foreign keys.
CREATE INDEX products_seller_idx ON products(seller_id);
CREATE INDEX orders_promo_idx ON orders(promo_code_id) WHERE promo_code_id IS NOT NULL;
CREATE INDEX order_items_product_idx ON order_items(product_id);
