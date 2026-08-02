ALTER TABLE printers ADD COLUMN connection_preference TEXT NOT NULL DEFAULT 'auto'
    CHECK (connection_preference IN ('auto', 'rfcomm', 'ble'));
