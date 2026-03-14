import unittest

from relay_bot.telegram import chunk_message


class ChunkMessageTests(unittest.TestCase):
    def test_chunks_stay_within_limit(self) -> None:
        text = "Paragraph one.\n\n" + ("x" * 50) + "\n" + ("y" * 50)
        chunks = chunk_message(text, 40)
        self.assertGreater(len(chunks), 1)
        self.assertTrue(all(len(chunk) <= 40 for chunk in chunks))

    def test_empty_message_has_placeholder(self) -> None:
        self.assertEqual(chunk_message("   ", 20), ["(empty response)"])


if __name__ == "__main__":
    unittest.main()
