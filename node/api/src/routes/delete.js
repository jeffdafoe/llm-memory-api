const { Router } = require('express');
const pool = require('../db');

const router = Router();

router.post('/delete', async (req, res) => {
    const { namespace, source_file } = req.body;

    if (!namespace || !source_file) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, source_file' }
        });
    }

    const result = await pool.query(
        'DELETE FROM chunks WHERE namespace = $1 AND source_file = $2',
        [namespace, source_file]
    );

    res.json({
        chunks_deleted: result.rowCount
    });
});

module.exports = router;
